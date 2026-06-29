package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/clems4ever/llmbox/internal/sandbox"
)

// Client is the host-side handle to one box's agent. It opens a fresh control
// connection per call via Dial, so concurrent operations don't contend on a
// single connection. The backend supplies Dial (an AF_UNIX dial to the box's
// bind-mounted socket for the container backend; a vsock dial later).
type Client struct {
	// Dial opens a new control connection to the box's agent.
	Dial func(ctx context.Context) (net.Conn, error)
}

// NewUnixClient returns a Client that dials the agent's Unix socket at path.
//
// @arg path The filesystem path of the box's control socket.
// @return *Client A client that opens a new connection per call.
//
// @testcase TestClientOverUnixSocket drives an agent through a unix-socket client.
func NewUnixClient(path string) *Client {
	return &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

// call opens a connection, sends one verb request, and decodes the response into
// out (which may be nil for verbs that return no payload).
//
// @arg ctx Context for dialing the agent.
// @arg verb The verb to invoke.
// @arg in The request payload to encode (nil for verbs that take none).
// @arg out The value to decode the response payload into (nil to discard).
// @error error if dialing, framing, or the verb itself fails.
//
// @testcase TestClientOverUnixSocket invokes every verb through call.
func (c *Client) call(ctx context.Context, verb string, in, out any) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return fmt.Errorf("connecting to box agent: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var data json.RawMessage
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		data = b
	}
	if err := writeFrame(conn, req{Verb: verb, Data: data}); err != nil {
		return fmt.Errorf("sending %s: %w", verb, err)
	}
	var r resp
	if err := readFrame(conn, &r); err != nil {
		return fmt.Errorf("reading %s response: %w", verb, err)
	}
	if r.Err != "" {
		return errors.New(r.Err)
	}
	if out != nil && len(r.Data) > 0 {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

// Init writes the box's per-create files and records its parameters.
//
// @arg ctx Context for the call.
// @arg in The init request.
// @error error if the call fails.
//
// @testcase TestClientOverUnixSocket initialises a box through the client.
func (c *Client) Init(ctx context.Context, in InitReq) error {
	return c.call(ctx, verbInit, in, nil)
}

// Start launches claude and returns the authorize URL (login needed) or session
// URL (already authenticated).
//
// @arg ctx Context for the call.
// @return StartResp The authorize or session URL.
// @error error if the call fails.
//
// @testcase TestClientOverUnixSocket starts a box and reads back its authorize URL.
func (c *Client) Start(ctx context.Context) (StartResp, error) {
	var out StartResp
	err := c.call(ctx, verbStart, nil, &out)
	return out, err
}

// SubmitCode feeds the OAuth code and returns the session URL.
//
// @arg ctx Context for the call.
// @arg code The OAuth code to submit.
// @return string The remote-control session URL.
// @error error if the call fails.
//
// @testcase TestClientOverUnixSocket submits the code and reads back the session URL.
func (c *Client) SubmitCode(ctx context.Context, code string) (string, error) {
	var out SubmitCodeResp
	err := c.call(ctx, verbSubmitCode, SubmitCodeReq{Code: code}, &out)
	return out.SessionURL, err
}

// Exec runs a command in the box and returns its captured result.
//
// @arg ctx Context for the call.
// @arg cmd The command and arguments to run.
// @return sandbox.ExecResult The command's output and exit code.
// @error error if the call fails.
//
// @testcase TestClientOverUnixSocket runs a command through Exec.
func (c *Client) Exec(ctx context.Context, cmd []string) (sandbox.ExecResult, error) {
	var out sandbox.ExecResult
	err := c.call(ctx, verbExec, ExecReq{Cmd: cmd}, &out)
	return out, err
}

// Logs returns the trailing console transcript of the box.
//
// @arg ctx Context for the call.
// @arg tail The maximum number of trailing lines (non-positive uses the agent default).
// @return string The trailing transcript.
// @error error if the call fails.
//
// @testcase TestClientOverUnixSocket reads back the box transcript through Logs.
func (c *Client) Logs(ctx context.Context, tail int) (string, error) {
	var out LogsResp
	err := c.call(ctx, verbLogs, LogsReq{Tail: tail}, &out)
	return out.Output, err
}

// DialPort opens a connection to a TCP port inside the box and returns it as a
// raw byte pipe (the agent splices it to localhost:port). The caller owns the
// returned connection and must close it.
//
// @arg ctx Context for dialing the agent.
// @arg port The TCP port inside the box to connect to.
// @return net.Conn A connection spliced to the in-box port.
// @error error if dialing the agent fails or the agent cannot reach the port.
//
// @testcase TestClientDialPort reaches a listener inside the box through the agent.
func (c *Client) DialPort(ctx context.Context, port int) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to box agent: %w", err)
	}
	data, _ := json.Marshal(DialReq{Port: port})
	if err := writeFrame(conn, req{Verb: verbDial, Data: data}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending dial: %w", err)
	}
	// The agent sends one response frame (open or error) before the raw splice.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var r resp
	if err := readFrame(conn, &r); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading dial response: %w", err)
	}
	if r.Err != "" {
		conn.Close()
		return nil, errors.New(r.Err)
	}
	// Clear the handshake deadline so the spliced pipe is not time-bounded.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
