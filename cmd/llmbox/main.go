// Command llmbox runs a server that manages sandboxed Claude containers
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
//	LLMBOX_CLAUDE_IMAGE       base image launched for each box (default "debian:bookworm-slim"); any glibc image works — Claude is injected, not baked in
//	LLMBOX_CLAUDE_BIN         path to the standalone Claude binary injected into each box (default "/opt/llmbox/claude")
//	LLMBOX_REMOTE_ARGS        args passed to `claude remote-control` (default "--spawn same-dir")
//	LLMBOX_AUTH_TTL_SECONDS   how long a box may stay un-authenticated (default 300)
//	LLMBOX_STATE_FILE         bbolt file persisting the session registry (default "llmbox-sessions.db")
//
// Box lifecycle hooks (optional). LLMBOX_HOOKS is a PATH-style list (separated by
// the OS path-list separator) of external executables llmbox runs at box.create
// and box.destroy, exchanging JSON per the hookproto contract. A hook may inject
// files into each box and persist opaque state; this is how integrations like
// granular plug in without llmbox depending on them. LLMBOX_BOX_PEERS is a
// comma-separated list of container names connected into every box's network so
// boxes can reach them (e.g. a hook's resource servers):
//
//	LLMBOX_HOOKS       OS-path-list of hook executables (e.g. "/opt/granular-llmbox/hook")
//	LLMBOX_BOX_PEERS   comma-separated container names every box can reach
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

	"github.com/clems4ever/llmbox/internal/docker"
	"github.com/clems4ever/llmbox/internal/hooks"
	"github.com/clems4ever/llmbox/internal/server"
)

const (
	name    = "llmbox"
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

	peers := splitCommaList(os.Getenv("LLMBOX_BOX_PEERS"))
	mgr, err := docker.NewManager(os.Getenv("LLMBOX_CLAUDE_IMAGE"), os.Getenv("LLMBOX_REMOTE_ARGS"), os.Getenv("LLMBOX_CLAUDE_BIN"), peers)
	if err != nil {
		return err
	}
	defer mgr.Close()

	// Optional box lifecycle hooks: external programs run at box.create/destroy.
	// New returns nil (no hooks) when LLMBOX_HOOKS is empty.
	hookRunner := hooks.New(splitPathList(os.Getenv("LLMBOX_HOOKS")))

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

	srv := server.New(mgr, hookRunner, publicURL, authTTL, store)
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

// splitPathList splits an OS-path-list (e.g. LLMBOX_HOOKS) into its non-empty,
// trimmed entries. The path-list separator (':' on Unix) is used so hook
// executable paths read naturally, like PATH.
//
// @arg spec The path-list-separated string (may be empty).
// @return []string The non-empty, trimmed entries, in order.
//
// @testcase TestSplitLists splits path-lists and comma-lists, dropping empties.
func splitPathList(spec string) []string {
	return splitAndTrim(spec, string(os.PathListSeparator))
}

// splitCommaList splits a comma-separated list (e.g. LLMBOX_BOX_PEERS) into its
// non-empty, trimmed entries.
//
// @arg spec The comma-separated string (may be empty).
// @return []string The non-empty, trimmed entries, in order.
//
// @testcase TestSplitLists splits path-lists and comma-lists, dropping empties.
func splitCommaList(spec string) []string {
	return splitAndTrim(spec, ",")
}

// splitAndTrim splits spec on sep, trims each entry, and drops empties, returning
// nil when nothing remains so an unset variable yields no entries.
//
// @arg spec The string to split.
// @arg sep The separator to split on.
// @return []string The non-empty, trimmed entries, in order.
//
// @testcase TestSplitLists exercises splitAndTrim via the two list helpers.
func splitAndTrim(spec, sep string) []string {
	var out []string
	for p := range strings.SplitSeq(spec, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
