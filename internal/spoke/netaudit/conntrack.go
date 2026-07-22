package netaudit

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Source produces a stream of flow Events for a Recorder to attribute. The real
// implementation shells out to conntrack(8); tests supply a fake so the recorder
// and its wiring are exercised without the host netfilter stack.
type Source interface {
	// Run streams events to emit until ctx is cancelled or an unrecoverable error
	// occurs. It is expected to be long-lived; a nil return means ctx ended.
	Run(ctx context.Context, emit func(Event)) error
}

// ConntrackSource streams the host conntrack event feed (`conntrack -E`). It reads
// the original- and reply-direction tuples and, when flow accounting is enabled,
// the per-direction byte counters — all of which are metadata the kernel already
// tracks; no packet is ever read. It needs the conntrack binary and CAP_NET_ADMIN
// (the same privilege the Firecracker egress path already requires), and degrades
// to a clean "no data" when either is absent.
type ConntrackSource struct {
	// command builds the conntrack invocation. It is a field so tests can inject a
	// fake command that prints recorded event lines. Nil uses the real conntrack.
	command func(ctx context.Context) *exec.Cmd
	// enableAcct turns on kernel flow accounting so byte counters are populated. It
	// is a field so tests can make it a no-op. Nil runs the real sysctl best-effort.
	enableAcct func(ctx context.Context)
}

// NewConntrackSource returns a Source backed by the real conntrack(8) event feed.
//
// @return *ConntrackSource A source that streams `conntrack -E`.
func NewConntrackSource() *ConntrackSource { return &ConntrackSource{} }

// Run streams conntrack events to emit, restarting the underlying feed with a
// short backoff if it exits, until ctx is cancelled. A conntrack that is missing
// or unprivileged simply keeps failing to start, so the loop idles harmlessly and
// the audit view stays empty rather than crashing the spoke.
//
// @arg ctx Context that ends the stream when cancelled.
// @arg emit The callback invoked for each decoded event.
// @error error is always nil; it returns only when ctx ends.
//
// @testcase TestConntrackSourceStreamsFakeCommand decodes events from a fake feed.
func (s *ConntrackSource) Run(ctx context.Context, emit func(Event)) error {
	s.enableAccounting(ctx)
	for ctx.Err() == nil {
		_ = s.runOnce(ctx, emit)
		// Backoff before restarting so a persistently-failing conntrack (absent
		// binary, no privilege) does not spin.
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// runOnce runs the feed until it exits or ctx ends.
func (s *ConntrackSource) runOnce(ctx context.Context, emit func(Event)) error {
	cmd := s.buildCommand(ctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	streamLines(stdout, emit)
	return cmd.Wait()
}

// buildCommand returns the conntrack event-feed command, or the injected fake.
func (s *ConntrackSource) buildCommand(ctx context.Context) *exec.Cmd {
	if s.command != nil {
		return s.command(ctx)
	}
	// -E: event mode; -o extended: stable, parseable field format. Byte counters
	// appear when net.netfilter.nf_conntrack_acct is on (enabled below).
	return exec.CommandContext(ctx, "conntrack", "-E", "-o", "extended")
}

// enableAccounting best-effort turns on conntrack flow accounting so byte counters
// are reported; without it flows are still audited, just without byte totals.
func (s *ConntrackSource) enableAccounting(ctx context.Context) {
	if s.enableAcct != nil {
		s.enableAcct(ctx)
		return
	}
	_ = exec.CommandContext(ctx, "sysctl", "-w", "net.netfilter.nf_conntrack_acct=1").Run()
}

// streamLines reads r line by line, decodes each into an Event, and emits the ones
// that parse. It returns when r reaches EOF (the command exited or the pipe
// closed).
//
// @arg r The conntrack feed's stdout.
// @arg emit The callback for each decoded event.
//
// @testcase TestConntrackSourceStreamsFakeCommand drives streamLines via a fake command.
func streamLines(r io.Reader, emit func(Event)) {
	sc := bufio.NewScanner(r)
	// conntrack lines are short, but raise the buffer cap so a busy line is never
	// truncated into an unparseable fragment.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ev, ok := parseConntrackLine(sc.Text()); ok {
			emit(ev)
		}
	}
}

// tcpStates is the set of conntrack TCP state names, used to pick the state token
// out of a line without positional assumptions.
var tcpStates = map[string]bool{
	"SYN_SENT": true, "SYN_RECV": true, "ESTABLISHED": true, "FIN_WAIT": true,
	"CLOSE_WAIT": true, "LAST_ACK": true, "TIME_WAIT": true, "CLOSE": true,
	"CLOSING": true, "LISTEN": true, "SYN_SENT2": true,
}

// parseConntrackLine decodes one conntrack event line into an Event. conntrack
// prints two tuples per flow — the original direction (the box is the source) then
// the reply direction — so the first src=/dst=/sport=/dport= describe the box's
// outbound connection and the first vs. second bytes= counter are the out/in byte
// totals. Lines for protocols or forms it does not understand return ok=false.
//
// @arg line One line of `conntrack -E -o extended` output.
// @return Event The decoded event.
// @return bool Whether the line was a parseable flow event.
//
// @testcase TestParseConntrackTCPDestroy extracts tuple, state, and both byte counters.
// @testcase TestParseConntrackUDPNew extracts a stateless UDP flow.
// @testcase TestParseConntrackIgnoresUnknown rejects non-flow and unknown-proto lines.
func parseConntrackLine(line string) (Event, bool) {
	line = strings.TrimSpace(line)
	var ev Event
	switch {
	case strings.HasPrefix(line, "[NEW]"):
		line = line[len("[NEW]"):]
	case strings.HasPrefix(line, "[UPDATE]"):
		line = line[len("[UPDATE]"):]
	case strings.HasPrefix(line, "[DESTROY]"):
		ev.Closed = true
		line = line[len("[DESTROY]"):]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return Event{}, false
	}
	switch fields[0] {
	case "tcp", "udp", "icmp", "icmpv6", "udplite", "dccp", "sctp":
		ev.Proto = fields[0]
	default:
		return Event{}, false // e.g. "unknown", or a non-flow line
	}
	// dir counts how many original-direction boundaries (src=) we have crossed: 1
	// while inside the original tuple, 2 once into the reply tuple. Fields before
	// the first src= (proto number, timeout, state, icmp type/code) are handled or
	// ignored positionally-free.
	dir := 0
	for _, tok := range fields[1:] {
		if tcpStates[tok] {
			if ev.State == "" {
				ev.State = tok
			}
			continue
		}
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "src":
			dir++
			if dir == 1 {
				ev.SrcIP = v
			}
		case "dst":
			if dir == 1 {
				ev.DstIP = v
			}
		case "sport":
			if dir == 1 {
				ev.SrcPort = atoi(v)
			}
		case "dport":
			if dir == 1 {
				ev.DstPort = atoi(v)
			}
		case "bytes":
			n := atou(v)
			switch dir {
			case 1:
				ev.BytesOut = n
			case 2:
				ev.BytesIn = n
			}
		}
	}
	if ev.SrcIP == "" || ev.DstIP == "" {
		return Event{}, false
	}
	return ev, true
}

// atoi parses a base-10 int, returning 0 on failure (a malformed port is simply
// unknown, not fatal to the line).
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// atou parses a base-10 unsigned counter, returning 0 on failure.
func atou(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
