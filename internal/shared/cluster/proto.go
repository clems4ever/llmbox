package cluster

import (
	"encoding/json"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// frameType tags every message on the wire.
type frameType string

const (
	// frameEnroll is the spoke's opening message: it carries either a join token
	// (first-time enrollment) or a saved name+credential (reconnect).
	frameEnroll frameType = "enroll"
	// frameWelcome is the hub's reply to a successful enroll. On first-time
	// enrollment it carries the minted per-spoke credential.
	frameWelcome frameType = "welcome"
	// frameReq is a hub→spoke verb request; frameResp is the spoke's reply,
	// correlated by ID. A non-empty Error on the response means the verb failed.
	frameReq  frameType = "req"
	frameResp frameType = "resp"
	// frameErr is a fatal protocol error (e.g. enrollment rejected); the sender
	// closes the connection after it.
	frameErr frameType = "err"
	// Stream frames carry a raw bidirectional byte tunnel to a box's port,
	// multiplexed over the same connection and correlated by ID (the stream ID).
	// frameStreamOpen (hub→spoke) opens the tunnel (Payload is a streamOpenReq);
	// frameStreamData carries bytes in either direction (Data); frameStreamClose
	// ends it in either direction (Error set when the open/dial failed). This is
	// what makes streaming proxying (WebSocket/SSE) to a box on a remote spoke work.
	frameStreamOpen  frameType = "stream_open"
	frameStreamData  frameType = "stream_data"
	frameStreamClose frameType = "stream_close"
	// frameSpokeReq is a spoke→hub verb request; frameSpokeResp is the hub's
	// reply, correlated by ID. It is the only spoke-originated RPC direction and
	// uses an ID space independent from the hub→spoke frameReq/frameResp pair, so
	// the two directions never confuse each other's correlation IDs. Today it
	// carries the box-port verbs a box invokes against its own spoke-stamped
	// identity (see BoxPortService).
	frameSpokeReq  frameType = "spoke_req"
	frameSpokeResp frameType = "spoke_resp"
)

// Verb method names carried in a frameReq.
const (
	methodCreate     = "create"
	methodSubmitCode = "submit_code"
	methodList       = "list"
	methodDestroy    = "destroy"
	methodPause      = "pause"
	methodResume     = "resume"
	methodLogs       = "logs"
	methodExec       = "exec"
	methodReap       = "reap"
)

// Verb method names carried in a frameSpokeReq (spoke→hub).
const (
	methodOpenBoxPort  = "open_box_port"
	methodCloseBoxPort = "close_box_port"
	methodListBoxPorts = "list_box_ports"
)

// frame is the single envelope exchanged over a cluster connection. Payload is
// the method-specific request or response JSON; Data carries raw stream bytes
// (base64 in JSON) for the stream frames; Error carries a verb-level, stream-open,
// or protocol-level failure message.
type frame struct {
	Type    frameType       `json:"type"`
	ID      uint64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Data    []byte          `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// enrollReq is the spoke's enrollment request. JoinToken is set for first-time
// enrollment; Name+Credential are set when reconnecting with a saved credential.
type enrollReq struct {
	JoinToken  string `json:"join_token,omitempty"`
	Name       string `json:"name,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// welcomeResp is the hub's reply to a successful enroll. Credential is set only
// on first-time enrollment (the spoke saves it and reconnects with it).
type welcomeResp struct {
	Name       string `json:"name"`
	Credential string `json:"credential,omitempty"`
}

// Per-verb request/response payloads. They mirror the BoxManager signatures.

type createReq struct {
	Opts sandbox.CreateOptions `json:"opts"`
}
type createResp struct {
	ID               string                `json:"id"`
	AuthorizeURL     string                `json:"authorize_url"`
	InitScriptFailed bool                  `json:"init_script_failed,omitempty"`
	InitScriptOutput string                `json:"init_script_output,omitempty"`
	PublishPorts     []sandbox.PublishPort `json:"publish_ports,omitempty"`
}

type submitCodeReq struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}
type submitCodeResp struct {
	SessionURL string `json:"session_url"`
}

type listResp struct {
	Boxes []sandbox.Box `json:"boxes"`
}

type destroyReq struct {
	IDOrName string `json:"id_or_name"`
}

type pauseReq struct {
	IDOrName string `json:"id_or_name"`
}

type resumeReq struct {
	IDOrName string `json:"id_or_name"`
}
type resumeResp struct {
	SessionURL string `json:"session_url"`
}

type logsReq struct {
	IDOrName string `json:"id_or_name"`
	Tail     int    `json:"tail"`
}
type logsResp struct {
	Logs string `json:"logs"`
}

type execReq struct {
	IDOrName string   `json:"id_or_name"`
	Cmd      []string `json:"cmd"`
}

type reapReq struct {
	TTLNanos int64 `json:"ttl_nanos"`
}
type reapResp struct {
	Reaped []string `json:"reaped"`
}

// Box-port payloads (spoke→hub, carried in a frameSpokeReq). The BoxID is
// stamped by the SPOKE from its own persisted record of which box the request
// arrived from — it is never taken from anything the box sent, which is what
// scopes a box's port requests to itself.

// BoxPortInfo is the box-facing view of one published port: just the port, its
// public URL, and the caller's description. It deliberately omits hub-side
// details (slug, spoke, creator) that a box has no business seeing.
type BoxPortInfo struct {
	Port        int    `json:"port"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type openBoxPortReq struct {
	BoxID       string `json:"box_id"`
	Port        int    `json:"port"`
	Description string `json:"description,omitempty"`
}
type openBoxPortResp struct {
	Port BoxPortInfo `json:"port"`
}

type closeBoxPortReq struct {
	BoxID string `json:"box_id"`
	Port  int    `json:"port"`
}

type listBoxPortsReq struct {
	BoxID string `json:"box_id"`
}
type listBoxPortsResp struct {
	Ports []BoxPortInfo `json:"ports"`
}

// streamOpenReq opens a raw byte tunnel to a box's port on the spoke, carried in
// a frameStreamOpen. The stream is identified by the frame's ID; subsequent
// frameStreamData/frameStreamClose frames with the same ID carry its bytes and
// its teardown. Unlike the buffered verbs, a tunnel streams live, so it proxies
// WebSocket and SSE to a box on a remote spoke.
type streamOpenReq struct {
	BoxID string `json:"box_id"`
	Port  int    `json:"port"`
}

// encodePayload marshals v into a frame payload. It panics only on a programmer
// error (a value that cannot be JSON-encoded), which none of the payloads are.
//
// @arg v The value to marshal.
// @return json.RawMessage The JSON encoding of v.
// @error error if v cannot be marshalled.
//
// @testcase TestFrameRoundTrip round-trips each payload type through encode/decode.
func encodePayload(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// decodePayload unmarshals a frame payload into v.
//
// @arg p The raw payload bytes.
// @arg v A pointer to the destination value.
// @error error if the payload cannot be unmarshalled into v.
//
// @testcase TestFrameRoundTrip decodes each payload type written by encodePayload.
func decodePayload(p json.RawMessage, v any) error {
	return json.Unmarshal(p, v)
}
