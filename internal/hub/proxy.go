package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
)

// boxDialer is the box-reachability capability the proxy needs from a spoke's box
// manager: open a connection to a port inside a box. A remote spoke satisfies it
// by opening a streaming tunnel over the cluster transport (cluster's remoteSpoke
// implements DialBox), so the reverse proxy streams — WebSocket and SSE included.
// Keeping this a separate, optional interface (not part of cluster.BoxManager)
// preserves the cluster protocol's box-verb RPC allowlist.
type boxDialer interface {
	DialBox(ctx context.Context, idOrName string, port int) (net.Conn, error)
}

// proxySlugLen is the number of random bytes behind a proxy slug; hex-encoded it
// yields a 24-character lowercase DNS label that is both a valid sub-domain and
// unguessable (so the URL itself is a weak capability on top of the auth gate).
const proxySlugLen = 12

// ProxyEnabled reports whether the HTTP proxy feature is configured (a base
// domain was set via SetProxyBaseDomain).
//
// @return bool True when proxying is enabled.
//
// @testcase TestCreateProxyDisabled reports disabled without a base domain.
func (s *Server) ProxyEnabled() bool { return s.proxyBaseDomain != "" }

// newProxySlug returns an unguessable lowercase-hex DNS label for a proxy
// sub-domain.
//
// @return string A 24-char hex slug.
// @error error if the system random source fails.
//
// @testcase TestCreateProxyRegistersAndBuildsURL checks a created proxy carries a slug.
func newProxySlug() (string, error) {
	b := make([]byte, proxySlugLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// proxyURL is the externally reachable URL of a proxy slug:
// <scheme>://<slug>.<base-domain>[:<port>]/, with the scheme and port taken from
// the public URL. The base domain is deliberately port-free (see config
// validation); the port is carried from public_url onto the advertised URL so a
// hub on a non-standard port (e.g. :8443) hands out a reachable URL. Incoming
// proxy Host matching strips the port, so this does not affect routing.
//
// @arg slug The proxy slug.
// @return string The absolute proxy URL, or "" when proxying is disabled.
//
// @testcase TestCreateProxyRegistersAndBuildsURL checks the built proxy URL.
// @testcase TestProxyURLCarriesPublicURLPort appends the public URL's port to the proxy URL.
func (s *Server) proxyURL(slug string) string {
	if s.proxyBaseDomain == "" {
		return ""
	}
	scheme := "https"
	if strings.HasPrefix(s.publicURL, "http://") {
		scheme = "http"
	}
	host := slug + "." + s.proxyBaseDomain
	// Carry the public URL's port onto the proxy host so the advertised URL is
	// reachable when the hub runs on a non-standard port.
	if u, err := url.Parse(s.publicURL); err == nil {
		if port := u.Port(); port != "" {
			host = net.JoinHostPort(host, port)
		}
	}
	return scheme + "://" + host + "/"
}

// isBrowserNavigation reports whether r is a top-level browser navigation — a GET
// for an HTML document that is not a WebSocket handshake. Only such requests are
// safe to answer with a redirect to the sign-in page; an XHR/fetch, WebSocket
// upgrade, or non-GET would be broken (or silently looped) by an HTML redirect.
//
// @arg r The incoming proxy request.
// @return bool True when r is a top-level HTML navigation that may be redirected.
//
// @testcase TestIsBrowserNavigation accepts an HTML GET and rejects XHR, WebSocket, and POST.
func isBrowserNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// signInURL builds the absolute sign-in page URL an unauthenticated proxy visitor
// is redirected to, carrying the current proxy request URL as the ?return= target
// so login bounces the visitor straight back to where they were.
//
// @arg r The incoming proxy request (its Host and URI form the return target).
// @return string The absolute {publicURL}/signin?return=<this request> URL.
//
// @testcase TestSignInURLCarriesReturn builds a sign-in URL whose return is the proxy request.
func (s *Server) signInURL(r *http.Request) string {
	scheme := "https"
	if strings.HasPrefix(s.publicURL, "http://") {
		scheme = "http"
	}
	ret := scheme + "://" + r.Host + r.URL.RequestURI()
	return s.publicURL + "/signin?return=" + url.QueryEscape(ret)
}

// createProxy enables an HTTP proxy to a box's port and returns the persisted
// record. It is idempotent: when a proxy for the same box and port already
// exists, that record is returned unchanged rather than a duplicate created (so a
// description supplied on a repeat call is ignored). The box must be a currently
// tracked box (looked up by its box ID); the port is validated. createdBy records
// who enabled the proxy (an admin email, or "" via the API) and description is an
// optional note stamped onto the record on first creation.
//
// @arg boxID The box ID of the box whose port to expose.
// @arg port The TCP port inside the box to forward to.
// @arg createdBy The identity enabling the proxy, or "" when unknown (API caller).
// @arg description An optional human-readable note for the proxy, or "" for none.
// @return store.ProxyRecord The new (or pre-existing) proxy record.
// @error error if proxying is disabled, the port is invalid, no box has that box ID, the box is terminated, or persistence fails.
//
// @testcase TestCreateProxyRegistersAndBuildsURL registers a proxy for a known box and stamps the description.
// @testcase TestCreateProxyDisabled errors when proxying is not enabled.
// @testcase TestCreateProxyUnknownBox errors when no box has the given box ID.
// @testcase TestCreateProxyRefusesTerminatedBox errors when the box is a terminated tombstone.
// @testcase TestCreateProxyRejectsBadPort rejects an out-of-range port.
// @testcase TestCreateProxyIdempotent returns the existing proxy for a repeated box/port on the same container.
// @testcase TestCreateProxyIdempotentKeepsDescription keeps the original description when a repeat create supplies a new one.
// @testcase TestCreateProxyReplacesStaleContainer mints a fresh slug when a same-id box has a new container.
func (s *Server) createProxy(boxID string, port int, createdBy, description string) (store.ProxyRecord, error) {
	if !s.ProxyEnabled() {
		return store.ProxyRecord{}, fmt.Errorf("proxying is not enabled on this server")
	}
	if port < 1 || port > 65535 {
		return store.ProxyRecord{}, fmt.Errorf("invalid port %d: must be between 1 and 65535", port)
	}
	sess := s.lookupByBoxID(boxID)
	if sess == nil {
		return store.ProxyRecord{}, fmt.Errorf("no box found with box ID %q (it may have expired, or was created without a box ID)", boxID)
	}
	// A tombstone's container is gone; a proxy to it could never route, and
	// minting one would only reserve a slug a future same-id box would replace.
	if sess.terminated() {
		return store.ProxyRecord{}, fmt.Errorf("box %q is terminated (it no longer exists on its spoke); recreate the box before enabling a proxy", boxID)
	}
	existing, err := s.findProxy(boxID, port)
	if err != nil {
		return store.ProxyRecord{}, err
	}
	if existing != nil {
		// Reuse the slug only when it belongs to the *same* box incarnation. A proxy
		// left over from an earlier box that happened to share this box ID points at
		// a destroyed incarnation, so it must not be handed back for the new box —
		// delete it and mint a fresh slug instead.
		if existing.InstanceID == sess.Generation {
			return *existing, nil
		}
		if derr := s.store.DeleteProxy(existing.Slug); derr != nil {
			return store.ProxyRecord{}, fmt.Errorf("replacing stale proxy: %w", derr)
		}
	}
	slug, err := newProxySlug()
	if err != nil {
		return store.ProxyRecord{}, fmt.Errorf("generating proxy slug: %w", err)
	}
	rec := store.ProxyRecord{
		Slug:        slug,
		BoxID:       boxID,
		InstanceID:  sess.Generation,
		Port:        port,
		Spoke:       sess.SpokeName,
		CreatedAt:   time.Now(),
		Owner:       createdBy,
		Description: description,
	}
	if err := s.store.SaveProxy(rec); err != nil {
		return store.ProxyRecord{}, fmt.Errorf("saving proxy: %w", err)
	}
	return rec, nil
}

// findProxy returns the proxy for a box and port, or nil when none exists.
//
// @arg boxID The box ID to match.
// @arg port The port to match.
// @return *store.ProxyRecord The matching proxy, or nil.
// @error error if the proxy list cannot be read.
//
// @testcase TestCreateProxyIdempotent finds the existing proxy for a box/port.
func (s *Server) findProxy(boxID string, port int) (*store.ProxyRecord, error) {
	list, err := s.store.ListProxies()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if strings.EqualFold(list[i].BoxID, boxID) && list[i].Port == port {
			return &list[i], nil
		}
	}
	return nil, nil
}

// listProxies returns the enabled proxies, optionally filtered to one box. An
// empty boxID returns every proxy.
//
// @arg boxID The box ID to filter by, or "" for all proxies.
// @return []store.ProxyRecord The matching proxies.
// @error error if the proxy list cannot be read.
//
// @testcase TestListProxiesFiltersByBox lists all proxies and filters by box ID.
func (s *Server) listProxies(boxID string) ([]store.ProxyRecord, error) {
	list, err := s.store.ListProxies()
	if err != nil {
		return nil, err
	}
	if boxID == "" {
		return list, nil
	}
	out := make([]store.ProxyRecord, 0, len(list))
	for _, p := range list {
		if strings.EqualFold(p.BoxID, boxID) {
			out = append(out, p)
		}
	}
	return out, nil
}

// deleteProxy disables the proxy for a box and port, returning the slug removed.
//
// @arg boxID The box ID of the proxy to remove.
// @arg port The port of the proxy to remove.
// @return string The slug of the removed proxy.
// @error error if no such proxy exists or the deletion fails.
//
// @testcase TestDeleteProxyRemoves removes a proxy by box and port.
// @testcase TestDeleteProxyUnknown errors when no proxy matches.
func (s *Server) deleteProxy(boxID string, port int) (string, error) {
	rec, err := s.findProxy(boxID, port)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", fmt.Errorf("no proxy found for box %q port %d", boxID, port)
	}
	if err := s.store.DeleteProxy(rec.Slug); err != nil {
		return "", fmt.Errorf("deleting proxy: %w", err)
	}
	return rec.Slug, nil
}

// deleteProxyBySlug disables the proxy with the given slug (used by the admin UI,
// which renders slugs). Deleting a missing slug is a no-op.
//
// @arg slug The proxy slug to remove.
// @error error if the deletion fails.
//
// @testcase TestDeleteProxyBySlug removes a proxy by its slug.
func (s *Server) deleteProxyBySlug(slug string) error {
	if err := s.store.DeleteProxy(slug); err != nil {
		return fmt.Errorf("deleting proxy: %w", err)
	}
	return nil
}

// deleteProxiesForBox best-effort removes every proxy pointing at a box, used
// when the box is destroyed or reaped so no dangling proxy outlives it. Errors
// are logged, not returned, since box teardown must proceed regardless.
//
// @arg boxID The box ID whose proxies to remove.
//
// @testcase TestDestroyBoxRemovesProxies removes a destroyed box's proxies.
func (s *Server) deleteProxiesForBox(boxID string) {
	if boxID == "" || !s.ProxyEnabled() {
		return
	}
	list, err := s.listProxies(boxID)
	if err != nil {
		s.logger().Warn("listing proxies to clean up box", "box", boxID, "err", err)
		return
	}
	for _, p := range list {
		if err := s.store.DeleteProxy(p.Slug); err != nil {
			s.logger().Warn("deleting proxy during box cleanup", "box", boxID, "slug", p.Slug, "err", err)
		}
	}
}

// proxySlugFromHost extracts the proxy slug from a request Host header when it is
// a single-label sub-domain of the configured base domain (e.g. host
// "ab12.proxy.example.com" with base "proxy.example.com" yields "ab12"). It
// returns ok=false when proxying is disabled or the host is not such a
// sub-domain, so the caller falls through to the normal UI/API routes.
//
// @arg host The request Host header (may include a port).
// @return string The extracted slug when matched.
// @return bool True when host is a proxy sub-domain of the base domain.
//
// @testcase TestProxySlugFromHost matches proxy sub-domains and rejects others.
func (s *Server) proxySlugFromHost(host string) (string, bool) {
	if s.proxyBaseDomain == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + strings.ToLower(s.proxyBaseDomain)
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// proxyAuthorized reports whether a request to a proxy may proceed. When auth is
// configured, the visitor must be signed in with an admin identity (the same gate
// the admin UI uses); the shared login cookie (see auth.cookie_domain) carries
// that session across the proxy sub-domains. When no provider is configured,
// proxying is open — matching the rest of the server, which then relies on a front
// reverse proxy for authn.
//
// @arg r The incoming proxy request.
// @return bool True when the request is authorized to use the proxy.
// @return int The HTTP status to reply with when not authorized (0 when authorized).
//
// @testcase TestHandleProxyRequiresLogin refuses an unauthenticated request when auth is on.
// @testcase TestHandleProxyForwards forwards when authorized.
func (s *Server) proxyAuthorized(r *http.Request) (bool, int) {
	if s.auth == nil {
		return true, 0
	}
	ls, ok := s.auth.CurrentLogin(r)
	if !ok {
		return false, http.StatusUnauthorized
	}
	if !ls.CanAdmin {
		return false, http.StatusForbidden
	}
	return true, 0
}

// handleProxy serves one request to a proxy sub-domain: it resolves the slug to
// an enabled proxy, authorizes the caller, locates the box's spoke, and reverse
// proxies the request to the box's port. It supports streaming, SSE, and
// WebSocket upgrades (httputil.ReverseProxy handles them over the box dialer's
// live tunnel to the spoke). A spoke that cannot dial the box is refused with 502.
//
// @arg w The response writer.
// @arg r The incoming request (its Host names the proxy).
// @arg slug The proxy slug parsed from the Host header.
//
// @testcase TestHandleProxyForwards proxies an authorized request to the box.
// @testcase TestHandleProxyUnknownSlug 404s a slug with no proxy.
// @testcase TestHandleProxyRequiresLogin 401s an unauthenticated request when auth is on.
// @testcase TestHandleProxyUnsupportedSpoke 502s a box on a spoke that cannot dial boxes.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, slug string) {
	rec, ok, err := s.store.GetProxy(slug)
	if err != nil {
		s.logger().Warn("reading proxy", "slug", slug, "err", err)
		http.Error(w, "proxy lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "Unknown or disabled proxy.", http.StatusNotFound)
		return
	}
	if allowed, code := s.proxyAuthorized(r); !allowed {
		// Bounce a browser navigation that merely lacks a session to the sign-in
		// page; it returns here once the shared login cookie is set. Anything that
		// isn't a top-level navigation (XHR, WebSocket) — and the signed-in-but-not-
		// authorized case (403) — gets the bare status instead, since redirecting it
		// to an HTML page would corrupt the response or loop.
		if code == http.StatusUnauthorized && s.auth != nil && isBrowserNavigation(r) {
			http.Redirect(w, r, s.signInURL(r), http.StatusFound)
			return
		}
		http.Error(w, "Unauthorized", code)
		return
	}

	mgr, err := s.spoke(rec.Spoke)
	if err != nil {
		http.Error(w, fmt.Sprintf("the box's spoke is not available: %v", err), http.StatusBadGateway)
		return
	}
	// The box is reached by dialing a live tunnel to its port through the spoke, so
	// the reverse proxy streams (WebSocket/SSE work). A spoke whose manager cannot
	// dial boxes is refused.
	transport, ok := s.boxTransport(mgr, rec)
	if !ok {
		http.Error(w, "this box's spoke does not support proxying", http.StatusBadGateway)
		return
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(rec.BoxID, strconv.Itoa(rec.Port))}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = transport
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		s.logger().Warn("proxy upstream failed", "slug", slug, "box", rec.BoxID, "port", rec.Port, "err", err)
		http.Error(w, "the box is not reachable on this port", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// boxTransport returns the http.RoundTripper the reverse proxy uses to reach a
// box: a streaming transport whose every dial opens a live tunnel to the box's
// port through the spoke. The second result is false when the spoke's manager
// cannot dial boxes (so proxying is refused).
//
// @arg mgr The resolved spoke's box manager.
// @arg rec The proxy record naming the target box and port.
// @return http.RoundTripper The transport reaching the box, or nil when unsupported.
// @return bool True when the spoke supports proxying.
//
// @testcase TestHandleProxyForwards uses the streaming transport.
// @testcase TestHandleProxyDialsByBoxID dials the box by its box ID.
// @testcase TestHandleProxyUnsupportedSpoke returns false when the spoke cannot dial boxes.
func (s *Server) boxTransport(mgr boxManager, rec store.ProxyRecord) (http.RoundTripper, bool) {
	dialer, ok := mgr.(boxDialer)
	if !ok {
		return nil, false
	}
	// Every outbound dial goes to the box itself, regardless of the synthetic
	// target host — the box has no host-published port, so it is reached through the
	// spoke's box dialer (a live tunnel over the cluster transport). This keeps the
	// connection streaming, so WebSocket and SSE work.
	//
	// Dial by box ID: the hub addresses boxes only by (spoke, box ID), and the
	// spoke's Find resolves the box ID to its current incarnation. A proxy only
	// ever exists for a box created with a box ID (createProxy requires a tracked
	// box), so rec.BoxID is always set. The generation token stamped on the record
	// is an opaque staleness key, never an address.
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialBox(ctx, rec.BoxID, rec.Port)
		},
		ResponseHeaderTimeout: 60 * time.Second,
	}, true
}
