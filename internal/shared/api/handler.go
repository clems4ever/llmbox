package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// NewHandler builds the HTTP handler serving b's box operations as the JSON box-
// control API, one POST route per Backend method. It is meant to be mounted on
// the server's mux under the /api/v1/ prefix.
//
// SECURITY — this handler performs no authentication of its own: it is pure
// transport. The hub mounts it behind its API auth middleware (an API key as a
// bearer token, or an admin login session with a CSRF header), so every route
// here is only reachable by an authenticated caller. Mounting it anywhere else
// without an equivalent gate exposes box exec/destroy to anyone who can reach it.
//
// @arg b The backend whose operations are exposed over HTTP.
// @return http.Handler A mux serving the box-control routes over b.
//
// @testcase TestBackendAPIRoundTrip drives every route through NewClient against NewHandler.
func NewHandler(b Backend) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST "+PathCreateBox, jsonHandler(func(ctx context.Context, req createBoxRequest) (createBoxResponse, error) {
		sess, err := b.CreateBox(ctx, req.Opts)
		return createBoxResponse{Session: sess}, err
	}))
	mux.Handle("POST "+PathAuthPageURL, jsonHandler(func(_ context.Context, req authPageURLRequest) (authPageURLResponse, error) {
		return authPageURLResponse{URL: b.AuthPageURL(req.Token)}, nil
	}))
	mux.Handle("POST "+PathLookupBox, jsonHandler(func(_ context.Context, req lookupBoxRequest) (lookupBoxResponse, error) {
		sess, ok := b.LookupByBoxID(req.BoxID)
		return lookupBoxResponse{Session: sess, Found: ok}, nil
	}))
	mux.Handle("POST "+PathListBoxes, jsonHandler(func(ctx context.Context, _ struct{}) (listBoxesResponse, error) {
		boxes, err := b.ListBoxes(ctx)
		return listBoxesResponse{Boxes: boxes}, err
	}))
	mux.Handle("POST "+PathSpokeStatuses, jsonHandler(func(ctx context.Context, _ struct{}) (spokeStatusesResponse, error) {
		spokes, err := b.SpokeStatuses(ctx)
		return spokeStatusesResponse{Spokes: spokes}, err
	}))
	mux.Handle("POST "+PathCreateSpoke, jsonHandler(func(ctx context.Context, req createSpokeRequest) (createSpokeResponse, error) {
		var ttl time.Duration
		if req.TTL != "" {
			var err error
			if ttl, err = time.ParseDuration(req.TTL); err != nil {
				return createSpokeResponse{}, fmt.Errorf("invalid ttl %q: %w", req.TTL, err)
			}
		}
		sp, err := b.CreateSpoke(ctx, req.Name, req.Backend, ttl)
		return createSpokeResponse{Spoke: sp}, err
	}))
	mux.Handle("POST "+PathDropSpoke, jsonHandler(func(ctx context.Context, req dropSpokeRequest) (emptyResponse, error) {
		return emptyResponse{}, b.DropSpoke(ctx, req.Name)
	}))
	mux.Handle("POST "+PathSetDefaultSpoke, jsonHandler(func(ctx context.Context, req setDefaultSpokeRequest) (emptyResponse, error) {
		return emptyResponse{}, b.SetDefaultSpoke(ctx, req.Name)
	}))
	mux.Handle("POST "+PathListJoinTokens, jsonHandler(func(ctx context.Context, _ struct{}) (listJoinTokensResponse, error) {
		tokens, err := b.ListJoinTokens(ctx)
		return listJoinTokensResponse{Tokens: tokens}, err
	}))
	mux.Handle("POST "+PathRevokeJoinToken, jsonHandler(func(ctx context.Context, req revokeJoinTokenRequest) (emptyResponse, error) {
		return emptyResponse{}, b.RevokeJoinToken(ctx, req.ID)
	}))
	mux.Handle("POST "+PathRegenerateJoinToken, jsonHandler(func(ctx context.Context, req regenerateJoinTokenRequest) (regenerateJoinTokenResponse, error) {
		sp, err := b.RegenerateJoinToken(ctx, req.ID)
		return regenerateJoinTokenResponse{Spoke: sp}, err
	}))
	mux.Handle("POST "+PathDestroyBox, jsonHandler(func(ctx context.Context, req destroyBoxRequest) (emptyResponse, error) {
		return emptyResponse{}, b.DestroyBox(ctx, req.BoxID)
	}))
	mux.Handle("POST "+PathPauseBox, jsonHandler(func(ctx context.Context, req pauseBoxRequest) (emptyResponse, error) {
		return emptyResponse{}, b.PauseBox(ctx, req.BoxID)
	}))
	mux.Handle("POST "+PathResumeBox, jsonHandler(func(ctx context.Context, req resumeBoxRequest) (emptyResponse, error) {
		return emptyResponse{}, b.ResumeBox(ctx, req.BoxID)
	}))
	mux.Handle("POST "+PathBoxLogs, jsonHandler(func(ctx context.Context, req boxLogsRequest) (boxLogsResponse, error) {
		logs, err := b.BoxLogs(ctx, req.BoxID, req.Tail)
		return boxLogsResponse{Logs: logs}, err
	}))
	mux.Handle("POST "+PathBoxExec, jsonHandler(func(ctx context.Context, req boxExecRequest) (boxExecResponse, error) {
		res, err := b.BoxExec(ctx, req.BoxID, req.Command)
		return boxExecResponse{Result: res}, err
	}))
	mux.Handle("POST "+PathProxyEnabled, jsonHandler(func(_ context.Context, _ struct{}) (proxyEnabledResponse, error) {
		return proxyEnabledResponse{Enabled: b.ProxyEnabled()}, nil
	}))
	mux.Handle("POST "+PathCreateProxy, jsonHandler(func(ctx context.Context, req createProxyRequest) (createProxyResponse, error) {
		p, err := b.CreateProxy(ctx, req.BoxID, req.Port, req.Description)
		return createProxyResponse{Proxy: p}, err
	}))
	mux.Handle("POST "+PathDeleteProxy, jsonHandler(func(ctx context.Context, req deleteProxyRequest) (emptyResponse, error) {
		return emptyResponse{}, b.DeleteProxy(ctx, req.BoxID, req.Port)
	}))
	mux.Handle("POST "+PathListProxies, jsonHandler(func(ctx context.Context, req listProxiesRequest) (listProxiesResponse, error) {
		proxies, err := b.ListProxies(ctx, req.BoxID)
		return listProxiesResponse{Proxies: proxies}, err
	}))

	return mux
}

// jsonHandler adapts a typed backend call into an HTTP handler: it decodes the
// JSON request body into Req (an empty body yields the zero Req, for the
// no-argument methods), invokes fn, and writes fn's result as JSON — or, if fn
// returns an error, a non-2xx response carrying the error message.
//
// @arg fn The typed backend call: it receives the decoded request and returns the response or an error.
// @return http.HandlerFunc A handler decoding Req, calling fn, and encoding the response or error.
//
// @testcase TestBackendAPIRoundTrip exercises jsonHandler through every registered route.
// @testcase TestHandlerRejectsBadJSON returns 400 when the request body is not valid JSON.
func jsonHandler[Req any, Resp any](fn func(context.Context, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		// A zero ContentLength means the no-argument methods (which POST an empty
		// body); decoding only when there is a body keeps req at its zero value.
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
				return
			}
		}
		resp, err := fn(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// writeJSON writes v as a JSON response with the given status code. An encode
// failure is unactionable once the header is sent, so it is ignored.
//
// @arg w The response writer.
// @arg status The HTTP status code to send.
// @arg v The value to encode as the JSON body.
//
// @testcase TestBackendAPIRoundTrip reads JSON bodies written by writeJSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an errorResponse with the given status code and message.
//
// @arg w The response writer.
// @arg status The non-2xx HTTP status code to send.
// @arg msg The error message the client surfaces.
//
// @testcase TestHandlerRejectsBadJSON checks the error body written for a bad request.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
