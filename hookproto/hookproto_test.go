package hookproto

import (
	"bytes"
	"strings"
	"testing"
)

// TestFileBytesDecodesBase64 checks Bytes decodes base64 content and passes
// plain text content through unchanged.
func TestFileBytesDecodesBase64(t *testing.T) {
	// "hi" base64-encoded.
	b, err := File{Path: "/x", ContentBase64: "aGk="}.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(b) != "hi" {
		t.Errorf("decoded = %q, want hi", b)
	}
	if b, _ := (File{Path: "/x", Content: "plain"}).Bytes(); string(b) != "plain" {
		t.Errorf("text = %q, want plain", b)
	}
	if _, err := (File{Path: "/x", ContentBase64: "!!!not base64"}).Bytes(); err == nil {
		t.Error("expected error for invalid base64")
	}
}

// TestFileModeParsesOctal checks FileMode parses an octal mode string and
// defaults to 0600 when the mode is empty.
func TestFileModeParsesOctal(t *testing.T) {
	if m, _ := (File{Mode: "0755"}).FileMode(); m != 0o755 {
		t.Errorf("mode = %o, want 0755", m)
	}
	if m, _ := (File{Mode: ""}).FileMode(); m != 0o600 {
		t.Errorf("default mode = %o, want 0600", m)
	}
	if _, err := (File{Mode: "98"}).FileMode(); err == nil {
		t.Error("expected error for non-octal mode")
	}
}

// TestServeRoundTrips checks Serve decodes a request, runs the handler with it,
// and encodes the returned response.
func TestServeRoundTrips(t *testing.T) {
	in := strings.NewReader(`{"event":"box.create","box":{"box_id":"h1"}}`)
	var out bytes.Buffer
	var gotEvent, gotHost string
	err := Serve(in, &out, func(req Request) (Response, error) {
		gotEvent, gotHost = req.Event, req.Box.BoxID
		return Response{State: "tok", Files: []File{{Path: "/a", Content: "x"}}}, nil
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if gotEvent != EventBoxCreate || gotHost != "h1" {
		t.Errorf("handler saw event=%q host=%q", gotEvent, gotHost)
	}
	if !strings.Contains(out.String(), `"state":"tok"`) || !strings.Contains(out.String(), `"path":"/a"`) {
		t.Errorf("response = %s", out.String())
	}
}

// TestServeSurfacesHandlerError checks a handler error is returned and no
// response is written.
func TestServeSurfacesHandlerError(t *testing.T) {
	var out bytes.Buffer
	err := Serve(strings.NewReader(`{"event":"box.destroy"}`), &out, func(Request) (Response, error) {
		return Response{}, errTest
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Errorf("wrote response on error: %s", out.String())
	}
}

var errTest = stringError("boom")

type stringError string

// Error returns the string error message, satisfying the error interface.
func (e stringError) Error() string { return string(e) }
