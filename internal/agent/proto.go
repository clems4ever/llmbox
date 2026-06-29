// Package agent implements the in-box guest agent and its host-side client. The
// agent runs inside a box (as the entrypoint, under tini), owns the `claude`
// process on a PTY, and serves the box-operation verbs over a Unix-domain
// control socket: Init (write per-box files), Start (launch claude and capture
// the OAuth authorize URL or, if already authenticated, the session URL),
// SubmitCode (feed the OAuth code and capture the session URL), Exec, Logs, and
// Dial (a data-plane verb that splices the connection to a localhost port inside
// the box). The host reaches the socket through a per-box bind mount, so the
// same client drives any backend — container today, microVM or remote VM later —
// without host→box bridge networking.
package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// The control-plane verbs. Each is the Verb of a request frame; Dial is special
// in that, after its response, the connection becomes a raw byte pipe.
const (
	verbInit       = "init"
	verbStart      = "start"
	verbSubmitCode = "submit_code"
	verbExec       = "exec"
	verbLogs       = "logs"
	verbDial       = "dial"
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

// resp is the envelope the agent sends back. Err is non-empty when the verb
// failed (and then Data is nil); otherwise Data carries the verb's JSON-encoded
// response payload (nil for verbs that return none). For Dial, an empty-Err resp
// signals that the localhost connection is open and the conn is now a raw pipe.
type resp struct {
	Err  string          `json:"err,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// InitReq carries everything the agent needs to prepare the box before launching
// claude: the per-box files to write, the remote-control args, the box ID (used
// to name the default session), and the environment for the claude process.
type InitReq struct {
	Files      []sandbox.InjectFile `json:"files,omitempty"`
	RemoteArgs string               `json:"remote_args,omitempty"`
	BoxID      string               `json:"box_id,omitempty"`
	Env        []string             `json:"env,omitempty"`
}

// StartResp reports the outcome of launching claude: exactly one field is set.
// AuthorizeURL means the box needs an OAuth login (the caller must follow up with
// SubmitCode); SessionURL means the box already had credentials and went straight
// to a ready remote-control session.
type StartResp struct {
	AuthorizeURL string `json:"authorize_url,omitempty"`
	SessionURL   string `json:"session_url,omitempty"`
}

// SubmitCodeReq carries the OAuth code to write to claude's login prompt.
type SubmitCodeReq struct {
	Code string `json:"code"`
}

// SubmitCodeResp carries the remote-control session URL printed once login
// completes.
type SubmitCodeResp struct {
	SessionURL string `json:"session_url"`
}

// ExecReq is a command to run inside the box.
type ExecReq struct {
	Cmd []string `json:"cmd"`
}

// LogsReq requests the last Tail lines of the box's console transcript; a
// non-positive Tail uses the agent default.
type LogsReq struct {
	Tail int `json:"tail"`
}

// LogsResp carries the requested transcript tail.
type LogsResp struct {
	Output string `json:"output"`
}

// DialReq names a TCP port on localhost inside the box to splice the control
// connection to.
type DialReq struct {
	Port int `json:"port"`
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
