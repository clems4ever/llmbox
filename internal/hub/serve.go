package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/config"
	"github.com/clems4ever/llmbox/internal/hub/hooks"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// Serve assembles and serves the llmbox hub from cfg: it opens the session store,
// sets up optional lifecycle hooks and activation auth, attaches the cluster hub
// (spokes join over the cluster transport and run every box), enables HTTP
// proxying of box ports, restores persisted sessions, starts the orphan reaper,
// and serves the box-control API and the UI on one HTTP port until the parent
// context is cancelled (SIGINT/SIGTERM) at which point it shuts down gracefully.
// The hub runs no box backend of its own — boxes run only on remote spokes. name
// and version label the startup banner.
//
// @arg parent The parent context whose cancellation (or a termination signal) triggers graceful shutdown.
// @arg cfg The resolved configuration driving the store, auth, clustering, and HTTP server.
// @arg name The binary name shown in the startup banner.
// @arg version The build version shown in the startup banner.
// @error error if the store or authenticator cannot be built, a state directory cannot be created, or the HTTP server fails for a reason other than a clean shutdown.
//
// @testcase TestServe starts the hub, serves the health endpoint, and shuts down on context cancel.
func Serve(parent context.Context, cfg *config.Config, name, version string) error {
	authTTL := time.Duration(cfg.AuthTTL)

	// Optional box lifecycle hooks: external programs run at box.create/destroy.
	// New returns nil (no hooks) when the list is empty.
	hookRunner := hooks.New(cfg.Hooks)

	// Persist the session registry so auth links survive a server restart. The
	// directory is 0700: the state file holds session data and the hashes of
	// bearer secrets, so no other local user should be able to traverse into it.
	if dir := filepath.Dir(cfg.StateFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	store, err := OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("closing session store: %v", err)
		}
	}()

	// Activation auth (OIDC). Returns nil when no provider is configured, which
	// leaves box activation unauthenticated.
	authr, err := auth.New(parent, cfg.Auth)
	if err != nil {
		return err
	}
	if authr == nil {
		log.Print("activation auth is DISABLED: anyone with a box's auth-page URL can activate it; configure auth.google to require sign-in")
	}

	// Cancel background work on signal (or when the parent context fires).
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := New(hookRunner, cfg.PublicURL, authTTL, store, authr)
	srv.SetProxyBaseDomain(cfg.Proxy.BaseDomain)
	if cfg.Proxy.BaseDomain != "" {
		log.Printf("box HTTP proxying enabled at *.%s", cfg.Proxy.BaseDomain)
	}

	// The hub runs no box backend of its own: every box runs on an independently
	// started spoke (llmbox-spoke) that joins over the cluster transport. Always
	// accept spoke connections, and route an unqualified box create to the default
	// spoke an admin picks in the UI.
	// srv implements cluster.BoxPortService, so boxes can publish their own
	// ports through their spoke's connection (identity enforced hub-side).
	srv.SetHub(cluster.NewHub(ctx, store, nil, nil, srv))
	log.Printf("spokes may join at %s/spoke/connect", cfg.PublicURL)

	// One HTTP server for everything: the box-control JSON API (under /api/v1/) and
	// the UI (auth pages, admin, health). The MCP protocol is served by the
	// separate llmbox-mcp binary, which forwards to the box-control API.
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: srv.APIHandler(),
		// SubmitCode blocks while the box logs in, so allow long requests.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}

	// Reload the box records saved before the restart. This reads only the store —
	// no spoke is contacted at startup; the periodic sync pass reconciles the
	// records with what the spokes actually run once they (re)connect.
	if n, err := srv.Restore(); err != nil {
		log.Printf("restore: %v", err)
	} else if n > 0 {
		log.Printf("restored %d session(s) from %s", n, cfg.StateFile)
	}

	go srv.ReapLoop(ctx, 30*time.Second, func(msg string) { log.Print(msg) })

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Printf("graceful shutdown failed for %s: %v", httpSrv.Addr, err)
		}
	}()

	scheme := "HTTP"
	if cfg.TLS.Enabled {
		scheme = "HTTPS"
	} else {
		// Loud, un-missable banner: serving the box-control API and relayed OAuth
		// codes in the clear is only safe behind a TLS-terminating proxy.
		for _, line := range insecureTransportWarning() {
			log.Print(line)
		}
	}
	log.Printf("%s %s listening on %s over %s (public URL %s, auth TTL %s)", name, version, cfg.HTTPAddr, scheme, cfg.PublicURL, authTTL)

	return listenAndServe(httpSrv, cfg.TLS)
}

// insecureTransportWarning returns the multi-line banner logged at startup when
// the server serves plaintext HTTP (TLS disabled). It is loud on purpose: API
// keys, login cookies, and relayed OAuth codes all cross this port, so running
// without TLS is only safe behind a TLS-terminating reverse proxy.
//
// @return []string The banner lines, each logged on its own line.
//
// @testcase TestInsecureTransportWarning returns a non-empty banner mentioning TLS.
func insecureTransportWarning() []string {
	return []string{
		"!! ==================================================================== !!",
		"!!  WARNING: serving over PLAINTEXT HTTP -- traffic is NOT encrypted.    !!",
		"!!                                                                      !!",
		"!!  API keys, login cookies, and relayed OAuth codes cross this port.   !!",
		"!!  Only run like this behind a TLS-terminating                         !!",
		"!!  reverse proxy on a trusted network. To terminate TLS in-process,    !!",
		"!!  set tls.enabled with tls.cert_file and tls.key_file in the config.  !!",
		"!! ==================================================================== !!",
	}
}

// listenAndServe serves httpSrv, terminating TLS in-process when tls.Enabled
// (loading its PEM certificate and key), otherwise serving plaintext HTTP. A
// clean shutdown (http.ErrServerClosed, triggered by graceful shutdown) is
// reported as success; any other listen error is wrapped with the address.
//
// @arg httpSrv The configured HTTP server to run (its Addr labels errors).
// @arg tls The TLS settings deciding HTTPS vs plaintext and the cert/key paths.
// @error error if the server stops for any reason other than a clean shutdown.
//
// @testcase TestListenAndServeTLS serves HTTPS with a generated cert and key.
// @testcase TestListenAndServePlaintext serves plaintext HTTP when TLS is disabled.
// @testcase TestListenAndServeTLSMissingCert errors when the cert file is absent.
func listenAndServe(httpSrv *http.Server, tls config.TLSConfig) error {
	if tls.Enabled {
		if err := httpSrv.ListenAndServeTLS(tls.CertFile, tls.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listening on %s: %w", httpSrv.Addr, err)
		}
		return nil
	}
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listening on %s: %w", httpSrv.Addr, err)
	}
	return nil
}
