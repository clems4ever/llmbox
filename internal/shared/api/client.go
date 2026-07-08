package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// Client is a Backend that forwards every call to a remote box-control API. It
// lets a process drive boxes on an upstream llmbox server over HTTP with no
// in-process access to Docker, the store, or the cluster — the llmbox-mcp binary
// wraps one to serve those operations as MCP tools.
type Client struct {
	base string
	hc   *http.Client
	// apiKey, when set, is sent as a bearer token on every request — the
	// credential the server's API auth middleware expects from a headless caller.
	apiKey string
}

// Compile-time check that Client satisfies the Backend contract.
var _ Backend = (*Client)(nil)

// NewClient builds a Client targeting the box-control API at baseURL (the upstream
// llmbox server's address). A nil hc uses http.DefaultClient.
//
// @arg baseURL The upstream server's base URL, e.g. http://llmbox:8081.
// @arg hc The HTTP client to use; nil uses http.DefaultClient.
// @return *Client A backend forwarding calls to baseURL.
//
// @testcase TestBackendAPIRoundTrip builds a client this way and drives every method.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// SetAPIKey makes the client authenticate every request with key as a bearer
// token (Authorization: Bearer <key>). An empty key sends no credential.
//
// @arg key The API key minted with `llmbox-server apikey add`, or "" for none.
//
// @testcase TestClientSendsAPIKey sends the key as a bearer token on requests.
func (c *Client) SetAPIKey(key string) { c.apiKey = key }

// post sends req as JSON to the client's path and decodes the JSON response into
// Resp. A non-2xx reply is turned into an error carrying the server's message.
//
// @arg ctx Context for the HTTP request.
// @arg c The client whose base URL and HTTP client are used.
// @arg path The API path to POST to.
// @arg req The request value to encode as the JSON body.
// @return Resp The decoded response value (zero on error).
// @error error if the request cannot be built/sent, the server returns non-2xx, or the response cannot be decoded.
//
// @testcase TestBackendAPIRoundTrip round-trips requests and responses through post.
// @testcase TestClientSurfacesServerError turns a server error body into a Go error.
func post[Req any, Resp any](ctx context.Context, c *Client, path string, req Req) (Resp, error) {
	var resp Resp
	body, err := json.Marshal(req)
	if err != nil {
		return resp, fmt.Errorf("encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	res, err := c.hc.Do(httpReq)
	if err != nil {
		return resp, fmt.Errorf("calling %s: %w", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		var e errorResponse
		_ = json.NewDecoder(res.Body).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return resp, errors.New(e.Error)
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return resp, fmt.Errorf("decoding response from %s: %w", path, err)
	}
	return resp, nil
}

// CreateBox launches a box on the upstream server and returns its session.
//
// @arg ctx Context for the request.
// @arg opts The image, box ID, description, and target spoke for the box.
// @return BoxSession The new box's ID, container ID, and auth token.
// @error error if the box cannot be created.
//
// @testcase TestBackendAPIRoundTrip creates a box through the client.
func (c *Client) CreateBox(ctx context.Context, opts sandbox.CreateOptions) (BoxSession, error) {
	r, err := post[createBoxRequest, createBoxResponse](ctx, c, PathCreateBox, createBoxRequest{Opts: opts})
	return r.Session, err
}

// AuthPageURL asks the upstream server for the auth page URL of token. Because the
// Backend contract returns no error, a transport failure yields "" — this is only
// ever called immediately after a successful CreateBox, so the upstream is up.
//
// @arg token The session token to build the auth page URL for.
// @return string The auth page URL, or "" if the upstream call fails.
//
// @testcase TestBackendAPIRoundTrip resolves an auth page URL through the client.
func (c *Client) AuthPageURL(token string) string {
	r, err := post[authPageURLRequest, authPageURLResponse](context.Background(), c, PathAuthPageURL, authPageURLRequest{Token: token})
	if err != nil {
		return ""
	}
	return r.URL
}

// LookupByBoxID resolves a box by its box ID on the upstream server.
//
// @arg boxID The box ID to look up.
// @return BoxSession The matching box's session (zero value when not found).
// @return bool Whether a box with that box ID exists.
//
// @testcase TestBackendAPIRoundTrip looks a box up by box ID through the client.
func (c *Client) LookupByBoxID(boxID string) (BoxSession, bool) {
	r, err := post[lookupBoxRequest, lookupBoxResponse](context.Background(), c, PathLookupBox, lookupBoxRequest{BoxID: boxID})
	if err != nil {
		return BoxSession{}, false
	}
	return r.Session, r.Found
}

// ListBoxes returns all boxes managed by the upstream server.
//
// @arg ctx Context for the request.
// @return []BoxView The managed boxes with their activation/session URLs.
// @error error if listing boxes fails.
//
// @testcase TestBackendAPIRoundTrip lists boxes through the client.
func (c *Client) ListBoxes(ctx context.Context) ([]BoxView, error) {
	r, err := post[struct{}, listBoxesResponse](ctx, c, PathListBoxes, struct{}{})
	return r.Boxes, err
}

// SpokeStatuses returns every spoke and its connection status from the upstream
// server.
//
// @arg ctx Context for the request.
// @return []SpokeStatus The spokes and their connection status.
// @error error if the spokes cannot be read.
//
// @testcase TestBackendAPIRoundTrip reads spoke statuses through the client.
func (c *Client) SpokeStatuses(ctx context.Context) ([]SpokeStatus, error) {
	r, err := post[struct{}, spokeStatusesResponse](ctx, c, PathSpokeStatuses, struct{}{})
	return r.Spokes, err
}

// CreateSpoke mints a one-time join token for a new spoke on the upstream server
// and returns it with the ready-to-run start command.
//
// @arg ctx Context for the request.
// @arg name The spoke name to enroll.
// @arg backend The box backend in the returned command ("docker" or "firecracker"; empty means docker).
// @arg ttl How long the token stays valid; <=0 uses the server default.
// @return SpokeEnrollment The token and start command.
// @error error if the token cannot be minted.
//
// @testcase TestBackendAPIRoundTrip creates a spoke enrollment through the client.
func (c *Client) CreateSpoke(ctx context.Context, name, backend string, ttl time.Duration) (SpokeEnrollment, error) {
	req := createSpokeRequest{Name: name, Backend: backend}
	if ttl > 0 {
		req.TTL = ttl.String()
	}
	r, err := post[createSpokeRequest, createSpokeResponse](ctx, c, PathCreateSpoke, req)
	return r.Spoke, err
}

// DropSpoke removes a spoke's enrollment on the upstream server.
//
// @arg ctx Context for the request.
// @arg name The spoke name to drop.
// @error error if the spoke cannot be dropped.
//
// @testcase TestBackendAPIRoundTrip drops a spoke through the client.
func (c *Client) DropSpoke(ctx context.Context, name string) error {
	_, err := post[dropSpokeRequest, emptyResponse](ctx, c, PathDropSpoke, dropSpokeRequest{Name: name})
	return err
}

// SetDefaultSpoke makes an enrolled spoke the default on the upstream server.
//
// @arg ctx Context for the request.
// @arg name The spoke name to make the default.
// @error error if the default cannot be set.
//
// @testcase TestBackendAPIRoundTrip sets the default spoke through the client.
func (c *Client) SetDefaultSpoke(ctx context.Context, name string) error {
	_, err := post[setDefaultSpokeRequest, emptyResponse](ctx, c, PathSetDefaultSpoke, setDefaultSpokeRequest{Name: name})
	return err
}

// ListJoinTokens returns every outstanding spoke join token from the upstream
// server.
//
// @arg ctx Context for the request.
// @return []JoinTokenInfo The outstanding join tokens.
// @error error if the tokens cannot be read.
//
// @testcase TestBackendAPIRoundTrip lists join tokens through the client.
func (c *Client) ListJoinTokens(ctx context.Context) ([]JoinTokenInfo, error) {
	r, err := post[struct{}, listJoinTokensResponse](ctx, c, PathListJoinTokens, struct{}{})
	return r.Tokens, err
}

// RevokeJoinToken deletes one outstanding join token by ID on the upstream
// server.
//
// @arg ctx Context for the request.
// @arg id The token ID to revoke.
// @error error if the token cannot be revoked.
//
// @testcase TestBackendAPIRoundTrip revokes a join token through the client.
func (c *Client) RevokeJoinToken(ctx context.Context, id string) error {
	_, err := post[revokeJoinTokenRequest, emptyResponse](ctx, c, PathRevokeJoinToken, revokeJoinTokenRequest{ID: id})
	return err
}

// DestroyBox stops and removes the box with the given container ID on the upstream
// server.
//
// @arg ctx Context for the request.
// @arg containerID The container ID of the box to destroy.
// @error error if the box cannot be destroyed.
//
// @testcase TestBackendAPIRoundTrip destroys a box through the client.
func (c *Client) DestroyBox(ctx context.Context, containerID string) error {
	_, err := post[destroyBoxRequest, emptyResponse](ctx, c, PathDestroyBox, destroyBoxRequest{ContainerID: containerID})
	return err
}

// BoxLogs returns the recent console output of a box by its box ID.
//
// @arg ctx Context for the request.
// @arg boxID The box ID of the box to read logs from.
// @arg tail The maximum number of trailing log lines to return.
// @return string The box's recent console output.
// @error error if the logs cannot be read.
//
// @testcase TestBackendAPIRoundTrip reads box logs through the client.
func (c *Client) BoxLogs(ctx context.Context, boxID string, tail int) (string, error) {
	r, err := post[boxLogsRequest, boxLogsResponse](ctx, c, PathBoxLogs, boxLogsRequest{BoxID: boxID, Tail: tail})
	return r.Logs, err
}

// BoxExec runs a shell command inside a box by its box ID.
//
// @arg ctx Context for the request.
// @arg boxID The box ID of the box to run the command in.
// @arg command The shell command line to run inside the box.
// @return sandbox.ExecResult The command's stdout, stderr, and exit code.
// @error error if the command cannot be run.
//
// @testcase TestBackendAPIRoundTrip runs a command through the client.
func (c *Client) BoxExec(ctx context.Context, boxID, command string) (sandbox.ExecResult, error) {
	r, err := post[boxExecRequest, boxExecResponse](ctx, c, PathBoxExec, boxExecRequest{BoxID: boxID, Command: command})
	return r.Result, err
}

// ProxyEnabled reports whether the upstream server has HTTP proxying configured. A
// transport failure is reported as disabled.
//
// @return bool True when proxying is enabled on the upstream server.
//
// @testcase TestBackendAPIRoundTrip reports proxy enablement through the client.
func (c *Client) ProxyEnabled() bool {
	r, err := post[struct{}, proxyEnabledResponse](context.Background(), c, PathProxyEnabled, struct{}{})
	if err != nil {
		return false
	}
	return r.Enabled
}

// CreateProxy enables an HTTP proxy to a box's port on the upstream server.
//
// @arg ctx Context for the request.
// @arg boxID The box ID whose port to expose.
// @arg port The port inside the box to forward to.
// @arg description An optional human-readable note for the proxy, or "" for none.
// @return ProxyInfo The new proxy's box ID, port, URL, slug, spoke, and description.
// @error error if the proxy cannot be created.
//
// @testcase TestBackendAPIRoundTrip creates a proxy through the client.
func (c *Client) CreateProxy(ctx context.Context, boxID string, port int, description string) (ProxyInfo, error) {
	r, err := post[createProxyRequest, createProxyResponse](ctx, c, PathCreateProxy, createProxyRequest{BoxID: boxID, Port: port, Description: description})
	return r.Proxy, err
}

// DeleteProxy disables the proxy for a box and port on the upstream server.
//
// @arg ctx Context for the request.
// @arg boxID The box ID of the proxy to remove.
// @arg port The port of the proxy to remove.
// @error error if the proxy cannot be removed.
//
// @testcase TestBackendAPIRoundTrip deletes a proxy through the client.
func (c *Client) DeleteProxy(ctx context.Context, boxID string, port int) error {
	_, err := post[deleteProxyRequest, emptyResponse](ctx, c, PathDeleteProxy, deleteProxyRequest{BoxID: boxID, Port: port})
	return err
}

// ListProxies returns the enabled proxies on the upstream server, optionally
// filtered to one box.
//
// @arg ctx Context for the request.
// @arg boxID The box ID to filter by, or "" for all proxies.
// @return []ProxyInfo The matching proxies.
// @error error if the proxies cannot be listed.
//
// @testcase TestBackendAPIRoundTrip lists proxies through the client.
func (c *Client) ListProxies(ctx context.Context, boxID string) ([]ProxyInfo, error) {
	r, err := post[listProxiesRequest, listProxiesResponse](ctx, c, PathListProxies, listProxiesRequest{BoxID: boxID})
	return r.Proxies, err
}
