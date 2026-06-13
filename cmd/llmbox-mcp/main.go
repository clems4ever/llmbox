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
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/clems4ever/llmbox-mcp/internal/docker"
	"github.com/clems4ever/llmbox-mcp/internal/server"
)

const (
	name    = "llmbox-mcp"
	version = "v0.1.0"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("%s: %v", name, err)
	}
}

func run() error {
	addr := envOr("LLMBOX_HTTP_ADDR", ":8080")
	publicURL := envOr("LLMBOX_PUBLIC_URL", "http://localhost:8080")
	authTTL := time.Duration(envInt("LLMBOX_AUTH_TTL_SECONDS", 300)) * time.Second

	mgr, err := docker.NewManager(os.Getenv("LLMBOX_CLAUDE_IMAGE"), os.Getenv("LLMBOX_REMOTE_ARGS"))
	if err != nil {
		return err
	}
	defer mgr.Close()

	srv := server.New(mgr, publicURL, authTTL)
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
