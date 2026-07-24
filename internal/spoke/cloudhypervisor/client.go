package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient talks to one box's Cloud Hypervisor VMM over its REST API Unix socket.
// The cloud-hypervisor process is launched with "--api-socket path=<sock>" and
// serves the OpenAPI control surface there; every call is an HTTP request tunnelled
// over that UDS. One client is bound to one box's socket.
type apiClient struct {
	sock string
	http *http.Client
}

// apiTimeout bounds a single control call. It is generous: vm.boot returns as soon
// as the VMM has started the vCPUs (the guest wait happens separately over vsock),
// so no call legitimately runs long.
const apiTimeout = 15 * time.Second

// newAPIClient builds a client bound to a VMM's API socket. The socket need not
// exist yet — it appears once cloud-hypervisor starts — so construction never
// touches the filesystem.
//
// @arg sock The box's Cloud Hypervisor API Unix-socket path.
// @return *apiClient A client whose calls tunnel HTTP over that socket.
//
// @testcase TestClientLifecycleCalls drives a fake VMM through a client built here.
func newAPIClient(sock string) *apiClient {
	return &apiClient{
		sock: sock,
		http: &http.Client{
			Timeout: apiTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

// call issues one Cloud Hypervisor API request over the box's socket and, when out
// is non-nil, decodes a JSON response body into it. Cloud Hypervisor answers control
// verbs with 204 No Content and readers (vm.info, vmm.ping) with 200 + JSON, so any
// other status is an error carrying the VMM's response body for context.
//
// @arg ctx Context bounding the call.
// @arg method The HTTP method ("PUT" for actions, "GET" for readers).
// @arg endpoint The API path under /api/v1 (e.g. "vm.boot").
// @arg body The request body to send, or nil.
// @arg out A pointer to decode a JSON response into, or nil to ignore the body.
// @error error if the request cannot be built or sent, or the VMM returns a non-2xx status.
//
// @testcase TestClientLifecycleCalls exercises call through the lifecycle verbs.
// @testcase TestClientCallError surfaces the VMM's body on a non-2xx status.
func (c *apiClient) call(ctx context.Context, method, endpoint string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshalling %s request: %w", endpoint, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+endpoint, rdr)
	if err != nil {
		return fmt.Errorf("building %s request: %w", endpoint, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s on %s: %w", endpoint, c.sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cloud-hypervisor %s returned %s: %s", endpoint, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding %s response: %w", endpoint, err)
		}
	}
	return nil
}

// createVM defines the box's VM on the VMM from cfg (it does not start the vCPUs;
// bootVM does).
//
// @arg ctx Context bounding the call.
// @arg cfg The box's VmConfig.
// @error error if the VMM rejects the config or is unreachable.
//
// @testcase TestClientLifecycleCalls creates a VM through this method.
func (c *apiClient) createVM(ctx context.Context, cfg vmConfig) error {
	return c.call(ctx, http.MethodPut, "vm.create", cfg, nil)
}

// bootVM starts the created VM's vCPUs.
//
// @arg ctx Context bounding the call.
// @error error if the VMM cannot boot the VM.
//
// @testcase TestClientLifecycleCalls boots a VM through this method.
func (c *apiClient) bootVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.boot", nil, nil)
}

// pauseVM freezes the VM's vCPUs while keeping its memory and devices, so resumeVM
// can continue it. It is the compute-freeze behind an instance Pause.
//
// @arg ctx Context bounding the call.
// @error error if the VMM cannot pause the VM.
//
// @testcase TestClientLifecycleCalls pauses a VM through this method.
func (c *apiClient) pauseVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.pause", nil, nil)
}

// resumeVM continues a paused VM's vCPUs.
//
// @arg ctx Context bounding the call.
// @error error if the VMM cannot resume the VM.
//
// @testcase TestClientLifecycleCalls resumes a VM through this method.
func (c *apiClient) resumeVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.resume", nil, nil)
}

// shutdownVMM stops the whole cloud-hypervisor process (guest and VMM), the clean
// way to end a box: the API socket goes away with the process. It is used to halt an
// orphaned VMM left by a previous spoke before removing the box's rootfs.
//
// @arg ctx Context bounding the call.
// @error error if the VMM cannot be reached or refuses to shut down.
//
// @testcase TestClientLifecycleCalls shuts a VMM down through this method.
func (c *apiClient) shutdownVMM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vmm.shutdown", nil, nil)
}

// ping reports whether the VMM answers on its API socket. A VMM started by a
// previous spoke survives that process's crash (it is orphaned, not killed), so a
// rehydrated box may well still be pingable.
//
// @arg ctx Context bounding the call.
// @return bool True when the VMM responds to vmm.ping.
//
// @testcase TestClientPing distinguishes a live VMM socket from a dead or missing one.
func (c *apiClient) ping(ctx context.Context) bool {
	return c.call(ctx, http.MethodGet, "vmm.ping", nil, nil) == nil
}
