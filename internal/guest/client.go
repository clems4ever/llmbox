package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// Client is the host-side handle to one box's guest. It opens a fresh control
// connection per call via Dial, so concurrent operations don't contend on a
// single connection. The backend supplies Dial via its Instance.Control: an
// AF_UNIX dial to the box's bind-mounted socket for the Docker backend, or a
// vsock CONNECT handshake over the hypervisor UDS for the Firecracker backend.
type Client struct {
	// Dial opens a new control connection to the box's guest.
	Dial func(ctx context.Context) (net.Conn, error)
}

// NewUnixClient returns a Client that dials the guest's Unix socket at path.
//
// @arg path The filesystem path of the box's control socket.
// @return *Client A client that opens a new connection per call.
//
// @testcase TestClientOverUnixSocket drives a guest through a unix-socket client.
func NewUnixClient(path string) *Client {
	return &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

// call opens a connection, sends one verb request, and decodes the response into
// out (which may be nil for verbs that return no payload).
//
// @arg ctx Context for dialing the guest.
// @arg verb The verb to invoke.
// @arg in The request payload to encode (nil for verbs that take none).
// @arg out The value to decode the response payload into (nil to discard).
// @error error if dialing, framing, or the verb itself fails.
//
// @testcase TestClientOverUnixSocket invokes every verb through call.
func (c *Client) call(ctx context.Context, verb string, in, out any) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return fmt.Errorf("connecting to box guest: %w", err)
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

// Init writes the box's per-create files, records its parameters, and runs the
// init script (if any). A file-write or already-initialised failure is a returned
// error; a failing init script is reported in the InitResp (ScriptFailed set),
// not as an error, so the caller can keep the box as a broken one to inspect.
//
// @arg ctx Context for the call.
// @arg in The init request.
// @return InitResp The init outcome; ScriptFailed is set (with the reason and output) when the init script fails.
// @error error if the call fails at the transport level (dial, framing, file write, or double-init).
//
// @testcase TestClientOverUnixSocket initialises a box through the client.
func (c *Client) Init(ctx context.Context, in InitReq) (InitResp, error) {
	var out InitResp
	err := c.call(ctx, verbInit, in, &out)
	return out, err
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
	err := c.call(ctx, verbExec, execReq{Cmd: cmd}, &out)
	return out, err
}

// PutFile streams a file into the box at an absolute path, owned by the box user.
// It sends a header frame naming the path, mode, and byte count, then writes
// exactly size bytes from r as a raw stream on the same connection (never
// embedding them in a control frame), and waits for the guest's acknowledgement.
// This is how a spoke stages a --copy file larger than the control frame's cap —
// the bytes stream straight from the host file to the box's disk. r must yield at
// least size bytes.
//
// @arg ctx Context for dialing and the transfer.
// @arg path The absolute in-box destination path.
// @arg mode The permission bits to set on the written file (0 means 0644).
// @arg size The exact number of bytes to stream from r.
// @arg r The source of the file's content.
// @return error if dialing, framing, streaming, or the guest-side write fails.
//
// @testcase TestClientPutFileStreams streams a file larger than the control frame and reads it back.
func (c *Client) PutFile(ctx context.Context, path string, mode, size int64, r io.Reader) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return fmt.Errorf("connecting to box guest: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	data, _ := json.Marshal(putFileReq{Path: path, Mode: mode, Size: size})
	if err := writeFrame(conn, req{Verb: verbPutFile, Data: data}); err != nil {
		return fmt.Errorf("sending putfile header for %s: %w", path, err)
	}
	if _, err := io.CopyN(conn, r, size); err != nil {
		return fmt.Errorf("streaming %s: %w", path, err)
	}
	// The guest acknowledges AFTER writing the file, so this response reports the
	// completed write (or its failure).
	var resp resp
	if err := readFrame(conn, &resp); err != nil {
		return fmt.Errorf("reading putfile response for %s: %w", path, err)
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	return nil
}

// DialPort opens a connection to a TCP port inside the box and returns it as a
// raw byte pipe (the guest splices it to localhost:port). The caller owns the
// returned connection and must close it.
//
// @arg ctx Context for dialing the guest.
// @arg port The TCP port inside the box to connect to.
// @return net.Conn A connection spliced to the in-box port.
// @error error if dialing the guest fails or the guest cannot reach the port.
//
// @testcase TestClientDialPort reaches a listener inside the box through the guest.
func (c *Client) DialPort(ctx context.Context, port int) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to box guest: %w", err)
	}
	data, _ := json.Marshal(dialReq{Port: port})
	if err := writeFrame(conn, req{Verb: verbDial, Data: data}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending dial: %w", err)
	}
	// The guest sends one response frame (open or error) before the raw splice.
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
