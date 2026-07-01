// Command llmbox-mcp is a stand-alone MCP server that exposes the llmbox tools
// and forwards every call to an upstream llmbox server's box-control API (mcpapi)
// over HTTP. It holds no Docker, session-store, or cluster state of its own — it
// is a thin protocol adapter, so it can run wherever a chatbot needs an MCP
// endpoint while the real work stays on the upstream server.
//
// It serves either a streamable-HTTP endpoint on --addr, or the MCP stdio
// transport (--stdio) for a chatbot that launches it as a child process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/clems4ever/llmbox/internal/mcpapi"
	"github.com/clems4ever/llmbox/internal/mcpserver"
)

const (
	name    = "llmbox-mcp"
	version = "v0.1.0"
)

// main parses flags, then serves the MCP endpoint until a termination signal
// arrives.
//
// @testcase TestRunHTTPServesAndStops covers the serve loop main delegates to.
func main() {
	upstream := flag.String("upstream", "", "upstream llmbox server box-control URL, e.g. http://llmbox:8081")
	addr := flag.String("addr", ":8081", "listen address for the streamable-HTTP MCP endpoint (ignored with --stdio)")
	stdio := flag.Bool("stdio", false, "serve MCP over stdio instead of HTTP (for a chatbot that launches this as a child process)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *upstream, *addr, *stdio); err != nil {
		log.Printf("%s exited: %v", name, err)
		os.Exit(1)
	}
}

// run builds the MCP tool server backed by an mcpapi client pointed at upstream,
// then serves it: over stdio when stdio is set (blocking until the client
// disconnects or ctx is cancelled), otherwise as a streamable-HTTP endpoint on
// addr with graceful shutdown when ctx is cancelled. It is the testable core of
// main.
//
// @arg ctx Context whose cancellation stops serving.
// @arg upstream The upstream llmbox server's box-control URL the client forwards to.
// @arg addr The listen address for the HTTP transport (ignored when stdio is true).
// @arg stdio Whether to serve over stdio instead of HTTP.
// @error error if upstream is empty, the HTTP listener fails for a reason other than a clean shutdown, or the stdio session ends in error.
//
// @testcase TestRunRequiresUpstream errors when no upstream URL is given.
// @testcase TestRunHTTPServesAndStops serves the HTTP endpoint and returns cleanly on cancel.
func run(ctx context.Context, upstream, addr string, stdio bool) error {
	if upstream == "" {
		return errors.New("--upstream is required (the llmbox server's box-control URL, e.g. http://llmbox:8081)")
	}

	srv := mcpserver.NewServer(mcpapi.NewClient(upstream, nil), name, version)

	if stdio {
		// stdio: the chatbot owns the process lifecycle; Run blocks until the client
		// closes the connection or the context is cancelled.
		return srv.Run(ctx, &mcp.StdioTransport{})
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// A create/exec call blocks on the upstream while the box works, so allow
		// long requests (mirrors the upstream server's own MCP timeouts).
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Printf("graceful shutdown failed for %s: %v", httpSrv.Addr, err)
		}
	}()

	log.Printf("%s %s listening on %s, forwarding to %s", name, version, addr, upstream)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	return nil
}
