package guest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// The host→box pty-control protocol multiplexed over the raw tunnel once a PTY is
// open. Each message is [type:1][len:4 big-endian][payload:len]; the box→host
// direction is unframed (the PTY's raw output). Framing only the host→box
// direction lets keystrokes and window resizes share the single connection while
// keeping the terminal-output path a plain byte stream the hub can splice.
const (
	// ptyMsgData carries raw stdin bytes to write to the PTY master.
	ptyMsgData byte = 0
	// ptyMsgResize carries a 4-byte payload (cols uint16, rows uint16, both
	// big-endian) requesting a window-size change.
	ptyMsgResize byte = 1
)

// ptyControlHeaderLen is the fixed size of a pty-control message header: one type
// byte plus a 4-byte big-endian length.
const ptyControlHeaderLen = 5

// maxPTYControlPayload bounds a single pty-control message so a malformed length
// prefix cannot make the guest allocate without limit. Terminal input arrives in
// tiny chunks, so this cap is generous.
const maxPTYControlPayload = 1 << 20

// defaultPTYCols and defaultPTYRows size a PTY whose open request names no size,
// so a shell always starts with a sane geometry even before the client's first
// resize.
const (
	defaultPTYCols = 80
	defaultPTYRows = 24
)

// handlePTY opens an interactive pseudo-terminal inside the box and splices it to
// the control connection. It launches the requested command (or a login shell)
// attached to a new PTY, running as the same unprivileged credential as the box's
// workload, then — after acknowledging the open — copies the PTY's raw output to
// the connection and consumes pty-control frames from it (stdin and resizes). It
// is terminal: the connection carries raw bytes afterwards and is closed when the
// PTY session ends.
//
// @arg conn The control connection to splice to the PTY.
// @arg data The JSON-encoded ptyReq (command and initial size).
//
// @testcase TestGuestPTYEchoesInput runs a shell through a PTY and round-trips input and output.
// @testcase TestGuestPTYResize applies a resize control frame to the PTY.
// @testcase TestGuestPTYRejectsBadRequest writes an error response for a malformed request.
func (a *Guest) handlePTY(conn net.Conn, data json.RawMessage) {
	var in ptyReq
	if err := json.Unmarshal(data, &in); err != nil {
		_ = writeFrame(conn, resp{Err: fmt.Sprintf("decoding pty: %v", err)})
		return
	}
	// Clear any handshake deadline the caller set: an interactive session is not
	// time-bounded, and a leftover deadline would abort a quiet terminal.
	_ = conn.SetDeadline(time.Time{})

	shell := a.ptyShell(in.Cmd)
	cmd := exec.Command(shell[0], shell[1:]...)
	cmd.Env = append(a.entryEnv(), "TERM=xterm-256color")
	if home := homeFromEnv(cmd.Env); home != "" {
		cmd.Dir = home
	}
	// A PTY needs its own session with the PTY as controlling terminal; carry the
	// box credential so the shell runs as the unprivileged box user (like Exec).
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if a.cred != nil {
		attrs.Credential = a.cred
	}
	size := &pty.Winsize{Cols: sizeOr(in.Cols, defaultPTYCols), Rows: sizeOr(in.Rows, defaultPTYRows)}
	ptmx, err := pty.StartWithAttrs(cmd, size, attrs)
	if err != nil {
		_ = writeFrame(conn, resp{Err: fmt.Sprintf("starting pty: %v", err)})
		return
	}
	defer func() {
		_ = ptmx.Close()
		// Reap the shell so a torn-down terminal never leaves a zombie.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if err := writeFrame(conn, resp{}); err != nil {
		return
	}
	a.splicePTY(conn, ptmx)
}

// ptyShell returns the command to run under the PTY: the caller's command when
// non-empty, else a login shell — the box user's $SHELL if set, otherwise bash,
// otherwise sh — so a terminal opens even on a minimal image.
//
// @arg cmd The caller-requested command (may be empty).
// @return []string The command and arguments to launch under the PTY.
//
// @testcase TestGuestPTYEchoesInput runs the default shell chosen here.
func (a *Guest) ptyShell(cmd []string) []string {
	if len(cmd) > 0 {
		return cmd
	}
	for _, e := range a.entryEnv() {
		if v, ok := strings.CutPrefix(e, "SHELL="); ok && v != "" {
			return []string{v, "-l"}
		}
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return []string{path, "-l"}
	}
	return []string{"/bin/sh", "-l"}
}

// splicePTY runs the bidirectional copy for an open PTY session: the PTY's raw
// output streams straight to conn, while conn is parsed as a sequence of
// pty-control frames whose data is written to the PTY and whose resizes are
// applied. It returns once either direction ends (the shell exits or the client
// disconnects), after which the caller tears the PTY down.
//
// @arg conn The control connection carrying pty-control frames in and raw output out.
// @arg ptmx The PTY master.
//
// @testcase TestGuestPTYEchoesInput moves bytes both ways through splicePTY.
func (a *Guest) splicePTY(conn net.Conn, ptmx *os.File) {
	done := make(chan struct{}, 2)
	// PTY output → connection (raw, unframed).
	go func() {
		_, _ = io.Copy(conn, ptmx)
		done <- struct{}{}
	}()
	// Connection → PTY (pty-control frames: data and resize).
	go func() {
		a.pumpPTYInput(conn, ptmx)
		done <- struct{}{}
	}()
	<-done
	// Closing both ends unblocks the still-running direction's copy.
	_ = ptmx.Close()
	_ = conn.Close()
	<-done
}

// pumpPTYInput reads pty-control frames off conn and applies them to ptmx: data
// frames are written to the PTY (as keystrokes), resize frames set the window
// size. It returns on the first read/parse error or when the connection closes.
//
// @arg conn The connection to read pty-control frames from.
// @arg ptmx The PTY master to write input to and resize.
//
// @testcase TestGuestPTYResize applies a resize frame read here.
func (a *Guest) pumpPTYInput(conn net.Conn, ptmx *os.File) {
	var hdr [ptyControlHeaderLen]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		msgType := hdr[0]
		n := binary.BigEndian.Uint32(hdr[1:])
		if n > maxPTYControlPayload {
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		switch msgType {
		case ptyMsgData:
			if _, err := ptmx.Write(payload); err != nil {
				return
			}
		case ptyMsgResize:
			if len(payload) == 4 {
				_ = pty.Setsize(ptmx, &pty.Winsize{
					Cols: binary.BigEndian.Uint16(payload[0:2]),
					Rows: binary.BigEndian.Uint16(payload[2:4]),
				})
			}
		default:
			// Unknown control type: ignore so a newer client stays compatible.
		}
	}
}

// sizeOr returns v when non-zero, else fallback, so an unset terminal dimension
// gets a sensible default.
//
// @arg v The requested dimension (0 means unset).
// @arg fallback The default to use when v is 0.
// @return uint16 v when non-zero, else fallback.
//
// @testcase TestGuestPTYEchoesInput starts a PTY sized through sizeOr.
func sizeOr(v, fallback uint16) uint16 {
	if v == 0 {
		return fallback
	}
	return v
}

// encodePTYControl builds one host→box pty-control message: a type byte, a
// big-endian length, and the payload. It is the encoder the host side (hub and
// tests) uses to frame terminal input and resizes for pumpPTYInput.
//
// @arg msgType The message type (ptyMsgData or ptyMsgResize).
// @arg payload The message payload.
// @return []byte The framed message.
//
// @testcase TestGuestPTYResize frames a resize with encodePTYControl.
func encodePTYControl(msgType byte, payload []byte) []byte {
	buf := make([]byte, ptyControlHeaderLen+len(payload))
	buf[0] = msgType
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[ptyControlHeaderLen:], payload)
	return buf
}

// encodePTYResize builds a resize control message for the given terminal size.
//
// @arg cols The terminal width in columns.
// @arg rows The terminal height in rows.
// @return []byte The framed resize message.
//
// @testcase TestGuestPTYResize builds the resize frame it then applies.
func encodePTYResize(cols, rows uint16) []byte {
	var p [4]byte
	binary.BigEndian.PutUint16(p[0:2], cols)
	binary.BigEndian.PutUint16(p[2:4], rows)
	return encodePTYControl(ptyMsgResize, p[:])
}
