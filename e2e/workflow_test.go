//go:build e2e

// Package e2e holds the end-to-end tests for llmbox. They wire the real server
// (box-control API + the admin web UI) on a real HTTP listener, drive the chatbot
// side over the real box-control HTTP API and the human side through a real
// browser via WebDriver, and simulate only the Docker box layer.
//
// Run them with the e2e build tag (they are excluded from the default unit suite):
//
//	make test-e2e        # or: go test -tags e2e ./e2e/...
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub"
	"github.com/clems4ever/llmbox/internal/hub/apikey"
	"github.com/clems4ever/llmbox/internal/shared/api"
)

// waitHealthy blocks until the server answers /healthz, so the test does not
// race the listener's startup.
func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy at %s: %v", base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// newBoxClient builds a box-control API client standing in for the chatbot: it
// mints an API key into the hub's store — exactly what a deployed programmatic
// caller is given — and returns a Client pointed at the server's box-control API,
// so calls travel over the real authenticated HTTP API to the server.
//
// @arg t The test, used for fatal errors.
// @arg base The server's box-control API base URL.
// @arg st The hub's store an API key is minted into.
// @return *api.Client A ready-to-use, authenticated box-control API client.
func newBoxClient(t *testing.T, base string, st hub.Store) *api.Client {
	t.Helper()
	key, err := apikey.Create(st, "e2e", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	c := api.NewClient(base, nil)
	c.SetAPIKey(key)
	return c
}
