// Command llmbox-mcp is a stand-alone MCP server that exposes the llmbox tools
// and forwards every call to an upstream llmbox server's box-control API
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
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/mcpserver"
	"github.com/clems4ever/llmbox/internal/shared/api"
)

const (
	name    = "llmbox-mcp"
	version = "v0.1.0"
)

// main executes the root command and exits non-zero on a fatal error.
//
// @testcase TestNewRootCmd covers the command wiring main relies on.
func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the llmbox-mcp command: it parses the upstream/addr/stdio
// flags and serves the MCP endpoint until interrupted.
//
// @return *cobra.Command The configured root command, ready to Execute.
//
// @testcase TestNewRootCmd checks the command wiring (use, flags, required upstream).
func newRootCmd() *cobra.Command {
	var (
		upstream string
		apiKey   string
		addr     string
		stdio    bool
	)
	rootCmd := &cobra.Command{
		Use:           name,
		Short:         "Run a stand-alone MCP server that forwards to an upstream llmbox server",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The key is a secret, so the environment variable is the primary channel
			// (a flag value leaks into process listings); the flag wins when both are set.
			if apiKey == "" {
				apiKey = os.Getenv("LLMBOX_API_KEY")
			}
			return run(cmd.Context(), upstream, apiKey, addr, stdio)
		},
	}
	rootCmd.Flags().StringVar(&upstream, "upstream", "", "upstream llmbox server box-control URL, e.g. http://llmbox:8081")
	rootCmd.Flags().StringVar(&apiKey, "api-key", "", "API key authenticating against the upstream server (defaults to $LLMBOX_API_KEY; mint one with `llmbox-server apikey add`)")
	rootCmd.Flags().StringVar(&addr, "addr", ":8081", "listen address for the streamable-HTTP MCP endpoint (ignored with --stdio)")
	rootCmd.Flags().BoolVar(&stdio, "stdio", false, "serve MCP over stdio instead of HTTP (for a chatbot that launches this as a child process)")
	return rootCmd
}

// run builds the MCP tool server backed by an api client pointed at upstream,
// then serves it: over stdio when stdio is set (blocking until the client
// disconnects or ctx is cancelled), otherwise as a streamable-HTTP endpoint on
// addr with graceful shutdown when a termination signal (or ctx) fires. It is the
// testable core of the command.
//
// @arg parent Base context; serving stops when it (or SIGINT/SIGTERM) fires.
// @arg upstream The upstream llmbox server's box-control URL the client forwards to.
// @arg apiKey The API key sent as a bearer token on upstream calls; "" sends none.
// @arg addr The listen address for the HTTP transport (ignored when stdio is true).
// @arg stdio Whether to serve over stdio instead of HTTP.
// @error error if upstream is empty, the HTTP listener fails for a reason other than a clean shutdown, or the stdio session ends in error.
//
// @testcase TestRunRequiresUpstream errors when no upstream URL is given.
// @testcase TestRunHTTPServesAndStops serves the HTTP endpoint and returns cleanly on cancel.
func run(parent context.Context, upstream, apiKey, addr string, stdio bool) error {
	if upstream == "" {
		return errors.New("--upstream is required (the llmbox server's box-control URL, e.g. http://llmbox:8081)")
	}

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := api.NewClient(upstream, nil)
	client.SetAPIKey(apiKey)
	srv := mcpserver.NewServer(client, name, version)

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
