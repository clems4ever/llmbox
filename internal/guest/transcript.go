package guest

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// ttyWidth is wide enough that the authorize URL prints on a single line so
	// the regex matches it unwrapped; ttyHeight is an ordinary height.
	ttyWidth  = 1000
	ttyHeight = 50

	// defaultLogTail is how many trailing transcript lines Logs returns when the
	// caller passes a non-positive tail.
	defaultLogTail = 200

	// maxExecOutput caps a single Exec's captured stdout/stderr so one command
	// cannot return an unbounded payload over the control socket.
	maxExecOutput = 64 << 10

	// transcriptCap bounds the retained console transcript; once it grows past
	// transcriptCap the oldest bytes are dropped down to transcriptKeep.
	transcriptCap  = 1 << 20
	transcriptKeep = 1 << 19
)

// authorizeURLRe matches the OAuth authorize URL claude prints during login. It
// is intentionally specific (the oauth/authorize path with PKCE+state params) so
// it never matches a plain session URL.
var authorizeURLRe = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?\S*code_challenge=\S*state=[A-Za-z0-9_\-]+`)

// sessionURLRe matches the remote-control session URL printed once a box is
// ready. It is broad, so callers must test authorizeURLRe first to avoid
// classifying an authorize URL as a session URL.
var sessionURLRe = regexp.MustCompile(`https://claude\.(?:ai|com)/[A-Za-z0-9/_?=&.\-]+`)

// ansiRe matches ANSI escape sequences and carriage returns emitted by the TUI.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][AB0]|[\r]`)

// stripANSI removes ANSI escape sequences and carriage returns so regexes match
// the text the TUI rendered rather than its control codes.
//
// @arg b The raw TUI output bytes.
// @return []byte The input with ANSI escape sequences and carriage returns removed.
//
// @testcase TestStripANSI checks ANSI and carriage-return removal.
func stripANSI(b []byte) []byte {
	return ansiRe.ReplaceAll(b, nil)
}

// lastLines returns up to the last n bytes of b, trimmed, as a single-spaced
// string (whitespace collapsed) suitable for an error message.
//
// @arg b The bytes to take the tail of.
// @arg n The maximum number of trailing bytes to keep.
// @return string The trimmed, single-spaced tail of b.
//
// @testcase TestTranscriptWaitForTimesOut surfaces a tail built by lastLines.
func lastLines(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(strings.Join(strings.Fields(string(b)), " "))
}

// transcript accumulates the box's console output (the PTY stream) and supports
// forward-consuming scans: each waitFor begins where the previous match ended, so
// a later scan for the session URL does not re-match the earlier authorize URL.
// It also serves Logs from the same buffer. It is safe for concurrent use.
type transcript struct {
	mu       sync.Mutex
	raw      []byte        // bounded; appended as the PTY produces output
	consumed int           // offset into stripANSI(raw) already scanned past
	closed   bool          // set once the PTY stream ends
	closeErr error         // why the stream ended (for diagnostics)
	notify   chan struct{} // closed and replaced on every append to wake waiters
}

// newTranscript returns an empty transcript ready to accept output.
//
// @return *transcript A transcript with an initialised wake channel.
//
// @testcase TestTranscriptWaitForFindsMatch scans a transcript built by newTranscript.
func newTranscript() *transcript {
	return &transcript{notify: make(chan struct{})}
}

// append adds PTY output to the transcript, bounding its memory and waking any
// blocked scans. When the buffer is trimmed, the consumed offset is shifted with
// it so forward scans stay aligned.
//
// @arg b The newly read PTY bytes.
//
// @testcase TestTranscriptWaitForFindsMatch feeds output through append.
func (t *transcript) append(b []byte) {
	t.mu.Lock()
	t.raw = append(t.raw, b...)
	if len(t.raw) > transcriptCap {
		drop := len(t.raw) - transcriptKeep
		t.raw = t.raw[drop:]
		if t.consumed -= drop; t.consumed < 0 {
			t.consumed = 0
		}
	}
	close(t.notify)
	t.notify = make(chan struct{})
	t.mu.Unlock()
}

// close marks the stream finished (e.g. the PTY hit EOF), waking blocked scans so
// they fail rather than hang.
//
// @arg err The error that ended the stream, if any.
//
// @testcase TestTranscriptWaitForStreamEnds fails a pending scan once close is called.
func (t *transcript) close(err error) {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		t.closeErr = err
		close(t.notify)
		t.notify = make(chan struct{})
	}
	t.mu.Unlock()
}

// waitForAny blocks until one of res matches the not-yet-consumed transcript or
// the timeout elapses. It returns the matched text and the index of the matching
// regexp, advancing the consume offset past the match so a later scan continues
// after it. res are checked in priority order, so a more specific pattern listed
// first wins over a broader one that also matches the same text.
//
// @arg res The regexps to match, in priority order.
// @arg timeout How long to wait for a match before giving up.
// @return match The matched text, or empty when none matched.
// @return idx The index in res of the matching regexp, or -1 when none matched.
// @return tail The trailing transcript captured on failure, for diagnostics.
// @error error if the stream ends or the timeout elapses before a match.
//
// @testcase TestTranscriptWaitForFindsMatch returns the first matching regexp and advances past it.
// @testcase TestTranscriptWaitForStreamEnds errors with a tail when the stream ends first.
// @testcase TestTranscriptWaitForTimesOut errors with a tail when no match arrives in time.
func (t *transcript) waitForAny(res []*regexp.Regexp, timeout time.Duration) (match string, idx int, tail string, err error) {
	deadline := time.After(timeout)
	for {
		t.mu.Lock()
		cleaned := stripANSI(t.raw)
		if t.consumed > len(cleaned) {
			t.consumed = len(cleaned)
		}
		window := cleaned[t.consumed:]
		for i, re := range res {
			if loc := re.FindIndex(window); loc != nil {
				t.consumed += loc[1]
				t.mu.Unlock()
				return string(window[loc[0]:loc[1]]), i, "", nil
			}
		}
		closed, cerr, ch := t.closed, t.closeErr, t.notify
		t.mu.Unlock()

		if closed {
			return "", -1, lastLines(cleaned, 600), fmt.Errorf("stream ended before match: %v", cerr)
		}
		select {
		case <-ch:
		case <-deadline:
			return "", -1, lastLines(cleaned, 600), fmt.Errorf("timed out after %s", timeout)
		}
	}
}

// logs returns the last tail lines of the transcript with ANSI stripped; a
// non-positive tail uses defaultLogTail.
//
// @arg tail The maximum number of trailing lines to return; non-positive uses defaultLogTail.
// @return string The trailing transcript lines, ANSI-stripped.
//
// @testcase TestTranscriptLogsTail returns only the trailing lines.
func (t *transcript) logs(tail int) string {
	if tail <= 0 {
		tail = defaultLogTail
	}
	t.mu.Lock()
	cleaned := string(stripANSI(t.raw))
	t.mu.Unlock()
	lines := strings.Split(strings.TrimRight(cleaned, "\n"), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n")
}

// capOutput truncates b to maxExecOutput, appending a marker when it overflows,
// so a single exec cannot return an unbounded payload.
//
// @arg b The captured output bytes.
// @return string The output, truncated with a marker when it exceeds maxExecOutput.
//
// @testcase TestExecCapsOutput truncates output past the cap and marks it.
func capOutput(b []byte) string {
	if len(b) <= maxExecOutput {
		return string(b)
	}
	return string(b[:maxExecOutput]) + "\n... [output truncated]"
}
