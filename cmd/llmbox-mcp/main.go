// Command llmbox-mcp runs a server that manages sandboxed Claude containers
// ("llmboxes") and lets an end user authenticate each one via OAuth in their
// browser — never routing the OAuth secret through the chatbot.
//
// One process serves two things on the same HTTP port:
//
//	/              MCP (streamable HTTP) — a chatbot creates/lists/destroys boxes
//	/auth/{token}  web page where the user pastes their OAuth code
//
// Boxes that are never authenticated are destroyed after a TTL.
//
// Configuration (environment variables):
//
//	LLMBOX_HTTP_ADDR          listen address (default ":8080")
//	LLMBOX_PUBLIC_URL         external base URL for auth links (default "http://localhost:8080")
//	LLMBOX_CLAUDE_IMAGE       image launched for each box (default "claude-remote")
//	LLMBOX_REMOTE_ARGS        args passed to `claude remote-control` (default "--spawn same-dir")
//	LLMBOX_AUTH_TTL_SECONDS   how long a box may stay un-authenticated (default 300)
//	LLMBOX_STATE_FILE         bbolt file persisting the session registry (default "llmbox-sessions.db")
//
// Granular authorization (optional; enabled only when both AS URL and admin
// token are set). When enabled, each box gets a freshly minted granular subject
// token injected at GRANULAR_SUBJECT_PATH for the in-box agent to request grants:
//
//	LLMBOX_GRANULAR_AS_URL            granular authorization server base URL
//	LLMBOX_GRANULAR_ADMIN_TOKEN_FILE  file holding the admin token used to mint/revoke subjects
//	LLMBOX_GRANULAR_SUBJECT_PATH      in-box path for the subject token (default "/home/node/.granular/subject_token")
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
	"github.com/clems4ever/llmbox-mcp/internal/granular"
	"github.com/clems4ever/llmbox-mcp/internal/server"
)

const (
	name    = "llmbox-mcp"
	version = "v0.1.0"
)

// main runs the server and exits non-zero on a fatal error.
//
// @testcase TestEnvHelpers covers the configuration helpers main relies on.
func main() {
	if err := run(); err != nil {
		log.Fatalf("%s: %v", name, err)
	}
}

// run wires up the Docker manager, session store, and HTTP server, then serves
// until interrupted.
//
// @error error if configuration, the store, or the HTTP server fails.
//
// @testcase TestEnvHelpers covers the configuration helpers run relies on.
func run() error {
	addr := envOr("LLMBOX_HTTP_ADDR", ":8080")
	publicURL := envOr("LLMBOX_PUBLIC_URL", "http://localhost:8080")
	authTTL := time.Duration(envInt("LLMBOX_AUTH_TTL_SECONDS", 300)) * time.Second
	stateFile := envOr("LLMBOX_STATE_FILE", "llmbox-sessions.db")

	mgr, err := docker.NewManager(os.Getenv("LLMBOX_CLAUDE_IMAGE"), os.Getenv("LLMBOX_REMOTE_ARGS"))
	if err != nil {
		return err
	}
	defer mgr.Close()

	// Optional granular integration: mint a subject per box. New returns nil
	// (integration disabled) unless both the AS URL and admin token are set.
	minter, err := granularMinter()
	if err != nil {
		return err
	}

	// Persist the session registry so auth links survive a server restart.
	if dir := filepath.Dir(stateFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	store, err := server.OpenStore(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv := server.New(mgr, minter, publicURL, authTTL, store)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(srv.MCPServer(name, version)),
		// SubmitCode blocks while the box logs in, so allow long requests.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}

	// Cancel background work on signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Reload sessions saved before a restart, dropping any whose box is gone.
	if n, err := srv.Restore(ctx); err != nil {
		log.Printf("restore: %v", err)
	} else if n > 0 {
		log.Printf("restored %d session(s) from %s", n, stateFile)
	}

	go srv.ReapLoop(ctx, 30*time.Second, func(msg string) { log.Print(msg) })

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("%s %s listening on %s (public URL %s, auth TTL %s)", name, version, addr, publicURL, authTTL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// envOr returns the environment variable key, or def when it is unset or empty.
//
// @arg key The environment variable name.
// @arg def The fallback value when key is unset or empty.
// @return string The variable's value, or def.
//
// @testcase TestEnvHelpers checks the set and fallback paths.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt returns the environment variable key parsed as an int, or def when it
// is unset, empty, or not a valid integer.
//
// @arg key The environment variable name.
// @arg def The fallback value when key is unset, empty, or unparseable.
// @return int The parsed value, or def.
//
// @testcase TestEnvHelpers checks the set, invalid, and fallback paths.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// granularMinter builds the granular subject minter from the environment. It
// returns nil (integration disabled) unless both LLMBOX_GRANULAR_AS_URL and a
// readable LLMBOX_GRANULAR_ADMIN_TOKEN_FILE are set.
//
// @return *granular.Minter The configured minter, or nil when granular is not configured.
// @error error if the admin token file is set but cannot be read.
//
// @testcase TestEnvHelpers covers the configuration helpers run relies on.
func granularMinter() (*granular.Minter, error) {
	asURL := os.Getenv("LLMBOX_GRANULAR_AS_URL")
	tokenFile := os.Getenv("LLMBOX_GRANULAR_ADMIN_TOKEN_FILE")
	if asURL == "" || tokenFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	return granular.New(granular.Config{
		ASURL:       asURL,
		AdminToken:  strings.TrimSpace(string(raw)),
		SubjectPath: os.Getenv("LLMBOX_GRANULAR_SUBJECT_PATH"),
	}), nil
}
