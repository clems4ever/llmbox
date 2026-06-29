package agent

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestFrameRoundTrips writes a frame and reads it back unchanged.
func TestFrameRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	in := req{Verb: verbExec, Data: []byte(`{"cmd":["echo","hi"]}`)}
	if err := writeFrame(&buf, in); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var out req
	if err := readFrame(&buf, &out); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if out.Verb != in.Verb || string(out.Data) != string(in.Data) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

// TestReadFrameRejectsOversizeLength rejects a length prefix over maxFrame.
func TestReadFrameRejectsOversizeLength(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxFrame+1)
	err := readFrame(bytes.NewReader(hdr[:]), &req{})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want 'too large'", err)
	}
}
