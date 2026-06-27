package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxProxyBody bounds a buffered proxied request or response body. The cluster
// proxy verb buffers each whole message into one frame, so this keeps a large
// upload or download from blowing past the frame limit (and bounds memory). It
// is generous for ordinary app traffic; streaming endpoints to a remote box are
// not supported by this verb (use a box on the local spoke for those).
const maxProxyBody = 32 << 20 // 32 MiB

// BoxDialer is the box-reachability capability the spoke-side proxy needs from
// its local box manager: open a connection to a port inside a box. The
// in-process *docker.Manager implements it. It is kept here (not on BoxManager)
// so the seven-verb RPC allowlist is unchanged and only a spoke that can dial
// boxes services proxy requests.
type BoxDialer interface {
	DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error)
}

// HTTPProxier forwards one buffered HTTP request to a box's port and returns the
// response. The hub-side remoteSpoke implements it by round-tripping the
// proxy_http verb to the spoke; the server's proxy handler uses it to reach a
// box on a remote spoke. Buffered (no streaming/WebSocket/SSE); a box on the
// local spoke is proxied directly with full streaming instead.
type HTTPProxier interface {
	ProxyHTTP(ctx context.Context, boxID string, port int, method, path string, header http.Header, body []byte) (status int, respHeader http.Header, respBody []byte, err error)
}

// roundTripToBox executes a buffered proxy request on the spoke: it dials the
// named box's port through the local dialer and performs the HTTP round trip,
// returning the buffered response. The dial resolves through the box layer's
// managed-only check (see docker.Manager.DialBox), so this verb can only ever
// reach a port inside a box the spoke created — never an arbitrary host address
// — which keeps it from becoming a generic forward proxy.
//
// @arg ctx Context bounding the dial and round trip.
// @arg d The local box dialer (the spoke's *docker.Manager).
// @arg in The buffered request to forward.
// @return proxyHTTPResp The buffered response (status, headers, body).
// @error error if the box cannot be reached or the round trip fails.
//
// @testcase TestRoundTripToBox forwards a request to a box server and returns its response.
// @testcase TestRoundTripToBoxDialError surfaces a dial failure.
func roundTripToBox(ctx context.Context, d BoxDialer, in proxyHTTPReq) (proxyHTTPResp, error) {
	// Build a one-shot client whose every dial reaches the box; the URL host is a
	// placeholder since DialBox ignores the address.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialBox(ctx, in.BoxID, in.Port)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()

	path := in.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, "http://box"+path, bytes.NewReader(in.Body))
	if err != nil {
		return proxyHTTPResp{}, fmt.Errorf("building proxy request: %w", err)
	}
	if in.Header != nil {
		req.Header = in.Header
	}

	client := &http.Client{Transport: tr, Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return proxyHTTPResp{}, fmt.Errorf("proxying to box %s:%d: %w", in.BoxID, in.Port, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyBody))
	if err != nil {
		return proxyHTTPResp{}, fmt.Errorf("reading box response: %w", err)
	}
	return proxyHTTPResp{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}
