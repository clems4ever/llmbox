package cluster

import (
	"reflect"
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// TestFrameRoundTrip checks each payload type encodes and decodes back to an
// equal value through the frame payload helpers.
func TestFrameRoundTrip(t *testing.T) {
	cases := []any{
		&createReq{Opts: sandbox.CreateOptions{BoxID: "b1", SpokeName: "edge", Files: []sandbox.InjectFile{{Path: "/x", Content: []byte("hi"), Mode: 0o600}}}},
		&createResp{ID: "abc123", AuthorizeURL: "https://auth"},
		&createResp{ID: "abc123", PublishPorts: []sandbox.PublishPort{{Port: 8080, Description: "claude-control"}, {Port: 3000}}},
		&createResp{ID: "abc123", InitScriptFailed: true, InitScriptOutput: "boom"},
		&submitCodeReq{ID: "abc123", Code: "code-xyz"},
		&submitCodeResp{SessionURL: "https://session"},
		&listResp{Boxes: []sandbox.Box{{InstanceID: "c1", BoxID: "b1", Spoke: "edge"}}},
		&destroyReq{IDOrName: "b1"},
		&logsReq{IDOrName: "b1", Tail: 50},
		&logsResp{Logs: "line1\nline2"},
		&execReq{IDOrName: "b1", Cmd: []string{"/bin/sh", "-c", "echo hi"}},
		&sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 2},
		&reapReq{TTLNanos: int64(90)},
		&reapResp{Reaped: []string{"a", "b"}},
		&enrollReq{JoinToken: "tok"},
		&welcomeResp{Name: "edge", Credential: "cred"},
		&streamOpenReq{BoxID: "web-box", Port: 8000},
	}
	for _, want := range cases {
		payload, err := encodePayload(want)
		if err != nil {
			t.Fatalf("encodePayload(%T): %v", want, err)
		}
		got := reflect.New(reflect.TypeOf(want).Elem()).Interface()
		if err := decodePayload(payload, got); err != nil {
			t.Fatalf("decodePayload(%T): %v", want, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("round trip %T: got %+v, want %+v", want, got, want)
		}
	}
}
