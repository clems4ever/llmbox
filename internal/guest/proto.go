// Package guest implements the in-box guest and its host-side client. The guest
// runs inside a box (as the entrypoint, under tini) and serves the box-operation
// verbs over a Unix-domain control socket: Init (write per-box files and run the
// host-provided init script), Exec (run a command), and Dial (a data-plane verb
// that splices the connection to a localhost port inside the box). The host
// reaches the socket through a per-box bind mount, so the same client drives any
// backend — container today, microVM or remote VM later — without host→box bridge
// networking. The box's own workload is installed and started by the init script,
// not by the guest.
package guest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// The control-plane verbs. Each is the Verb of a request frame; Dial is special
// in that, after its response, the connection becomes a raw byte pipe.
const (
	verbInit    = "init"
	verbExec    = "exec"
	verbDial    = "dial"
	verbPutFile = "putfile"
	verbPTY     = "pty"
)

// maxFrame bounds a single control frame so a malformed length prefix cannot make
// the peer allocate without limit. Control payloads (file injection aside) are
// small; injected files are individually well under this.
const maxFrame = 16 << 20

// req is the envelope the host sends for one verb. Data carries the verb's
// JSON-encoded request payload (nil for verbs that take none).
type req struct {
	Verb string          `json:"verb"`
	Data json.RawMessage `json:"data,omitempty"`
}

// resp is the envelope the guest sends back. Err is non-empty when the verb
// failed (and then Data is nil); otherwise Data carries the verb's JSON-encoded
// response payload (nil for verbs that return none). For Dial, an empty-Err resp
// signals that the localhost connection is open and the conn is now a raw pipe.
type resp struct {
	Err  string          `json:"err,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// InitReq carries everything the guest needs to provision the box: the per-box
// files to write, the environment for the box's processes, and an optional
// host-provided init script run once inside the box (with its own timeout) so a
// spoke can customise every box without rebuilding the image.
type InitReq struct {
	Files []sandbox.InjectFile `json:"files,omitempty"`
	Env   []string             `json:"env,omitempty"`
	// InitScript is an optional provisioning script run inside the box during Init,
	// as the same (unprivileged) user the box's workload runs as. Empty runs
	// nothing. A non-zero exit reports a broken box (see InitResp.ScriptFailed).
	InitScript []byte `json:"init_script,omitempty"`
	// InitScriptTimeout bounds how long the init script may run. A non-positive
	// value uses the guest default (defaultInitScriptTimeout).
	InitScriptTimeout time.Duration `json:"init_script_timeout,omitempty"`
}

// InitResp reports the outcome of Init. A file-write or already-initialised
// failure is a transport error (an error frame), not this payload; this payload
// reports the one failure the host must NOT treat as a torn-down box: a failing
// init script. When ScriptFailed is true the box was provisioned and is left
// running, and ScriptError/ScriptOutput carry the reason and the script's
// captured output so the host can surface a broken box the operator can inspect
// instead of a vanished one. The zero value (ScriptFailed false) means Init
// succeeded.
type InitResp struct {
	ScriptFailed bool   `json:"script_failed,omitempty"`
	ScriptError  string `json:"script_error,omitempty"`
	ScriptOutput string `json:"script_output,omitempty"`
}

// execReq is a command to run inside the box.
type execReq struct {
	Cmd []string `json:"cmd"`
}

// dialReq names a TCP port on localhost inside the box to splice the control
// connection to.
type dialReq struct {
	Port int `json:"port"`
}

// ptyReq opens an interactive pseudo-terminal inside the box. Cmd is the program
// (and args) to run under the PTY; when empty the guest picks a login shell.
// Cols/Rows are the initial terminal size (0 falls back to a default). Like Dial,
// the guest replies with one response frame (open or error) and then the
// connection becomes a raw byte tunnel to the PTY: box→host carries the PTY's raw
// output, while host→box carries pty-control frames (see the ptyMsg* protocol in
// pty.go) so keystrokes and window resizes can share the one connection.
type ptyReq struct {
	Cmd  []string `json:"cmd,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
}

// putFileReq is the header of a PutFile: it names the absolute in-box path to
// write, its permission bits, and the exact number of content bytes that follow
// the header frame as a raw stream on the same connection. Streaming the bytes
// raw (rather than embedding them in a JSON frame, as Files does) keeps a large
// --copy off the bounded control frame and out of memory, so a box can be seeded
// with content far larger than maxFrame. Like Dial, the connection carries raw
// bytes after the header; unlike Dial, the guest sends its response frame AFTER
// consuming them, so the response acknowledges the completed write.
type putFileReq struct {
	Path string `json:"path"`
	Mode int64  `json:"mode,omitempty"`
	Size int64  `json:"size"`
}

// writeFrame writes v as a length-prefixed JSON frame (4-byte big-endian length
// then the JSON body).
//
// @arg w The writer to emit the frame to.
// @arg v The value to JSON-encode as the frame body.
// @error error if encoding fails or the write fails.
//
// @testcase TestFrameRoundTrips writes a frame and reads it back unchanged.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding frame: %w", err)
	}
	if len(b) > maxFrame {
		return fmt.Errorf("frame too large: %d bytes", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readFrame reads one length-prefixed JSON frame and decodes it into v.
//
// @arg r The reader to consume the frame from.
// @arg v The value to decode the frame body into.
// @error error if the length prefix exceeds maxFrame, the read fails, or the body is not valid JSON for v.
//
// @testcase TestFrameRoundTrips reads back a frame written by writeFrame.
// @testcase TestReadFrameRejectsOversizeLength rejects a length prefix over maxFrame.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
