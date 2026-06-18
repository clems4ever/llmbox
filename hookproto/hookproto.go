// Package hookproto defines the JSON wire protocol spoken between llmbox and the
// external hook executables it runs at box lifecycle events. llmbox stays free of
// any knowledge of what a hook does (mint a token, install a CLI, ...): it only
// speaks this protocol over a subprocess's stdin/stdout.
//
// For each event, llmbox runs a configured hook executable, writes a single JSON
// Request to its stdin, and reads a single JSON Response from its stdout. A
// non-zero exit status means the hook failed; on box.create that fails the box,
// on box.destroy it is logged and ignored.
//
// Hook authors writing in Go can import this package for the wire types and use
// Main to implement a hook in a few lines; authors in any other language can
// speak the same JSON by hand.
package hookproto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Event names carried in a Request.
const (
	// EventBoxCreate fires before a new box starts. The hook may return files to
	// inject into the box and an opaque State string llmbox persists for the box.
	EventBoxCreate = "box.create"
	// EventBoxDestroy fires when a box is destroyed or reaped. llmbox replays the
	// State the box.create hook returned so the hook can undo what it did.
	EventBoxDestroy = "box.destroy"
)

// Request is the JSON object llmbox writes to a hook's stdin.
type Request struct {
	// Event is the lifecycle event, one of the Event* constants.
	Event string `json:"event"`
	// Box describes the box the event concerns.
	Box Box `json:"box"`
	// State is the opaque string this hook returned from the box's box.create
	// event; set only on box.destroy, empty otherwise.
	State string `json:"state,omitempty"`
}

// Box describes the box an event concerns. Fields are best-effort: box.destroy
// may carry only what llmbox still knows about a box.
type Box struct {
	Image       string `json:"image,omitempty"`
	BoxID       string `json:"box_id,omitempty"`
	Description string `json:"description,omitempty"`
}

// Response is the JSON object a hook writes to its stdout.
type Response struct {
	// Files are written into the box before it starts (box.create only).
	Files []File `json:"files,omitempty"`
	// State is an opaque string llmbox persists with the box and replays to this
	// hook's box.destroy event (box.create only). A granular hook puts the minted
	// subject token here so it can revoke it on destroy.
	State string `json:"state,omitempty"`
}

// File is one file a hook asks llmbox to inject into a box. Exactly one of
// Content (UTF-8 text) or ContentBase64 (binary, base64-encoded) carries the
// bytes. Mode is an octal string (e.g. "0755"); empty defaults to "0600". UID
// and GID set the owner, which matters when a file lands in a non-root user's
// home and must stay readable by that user.
type File struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Mode          string `json:"mode,omitempty"`
	UID           int    `json:"uid,omitempty"`
	GID           int    `json:"gid,omitempty"`
}

// Bytes resolves a File's content, decoding ContentBase64 when set and otherwise
// returning Content as raw bytes.
//
// @return []byte The file's decoded content.
// @error error if ContentBase64 is set but not valid base64.
//
// @testcase TestFileBytesDecodesBase64 decodes base64 content and passes text through.
func (f File) Bytes() ([]byte, error) {
	if f.ContentBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("decoding base64 content for %q: %w", f.Path, err)
		}
		return b, nil
	}
	return []byte(f.Content), nil
}

// FileMode parses a File's octal Mode string, defaulting to 0600 when empty.
//
// @return int64 The parsed file mode.
// @error error if Mode is set but is not a valid octal number.
//
// @testcase TestFileModeParsesOctal parses an octal mode and defaults when empty.
func (f File) FileMode() (int64, error) {
	if f.Mode == "" {
		return 0o600, nil
	}
	m, err := strconv.ParseInt(f.Mode, 8, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing mode %q for %q: %w", f.Mode, f.Path, err)
	}
	return m, nil
}

// Handler handles a single hook Request and returns the Response to write back.
type Handler func(Request) (Response, error)

// Serve reads one JSON Request from r, dispatches it to h, and writes h's JSON
// Response to w. It is the engine behind Main, factored out so it can be tested
// without touching the process's stdio.
//
// @arg r The reader to decode the Request from.
// @arg w The writer to encode the Response to.
// @arg h The handler invoked with the decoded Request.
// @error error if the Request cannot be decoded, h fails, or the Response cannot be encoded.
//
// @testcase TestServeRoundTrips decodes a request, runs the handler, and encodes the response.
// @testcase TestServeSurfacesHandlerError returns the handler's error without writing a response.
func Serve(r io.Reader, w io.Writer, h Handler) error {
	var req Request
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return fmt.Errorf("decoding hook request: %w", err)
	}
	resp, err := h(req)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return fmt.Errorf("encoding hook response: %w", err)
	}
	return nil
}

// Main runs a hook over the process's stdin/stdout and exits the process: 0 on
// success, 1 (with the error on stderr) on failure. A hook's main can be just
// `hookproto.Main(myHandler)`.
//
// @arg h The handler invoked with the request read from stdin.
//
// @testcase TestServeRoundTrips covers the Serve engine Main delegates to.
func Main(h Handler) {
	if err := Serve(os.Stdin, os.Stdout, h); err != nil {
		fmt.Fprintf(os.Stderr, "hook: %v\n", err)
		os.Exit(1)
	}
}
