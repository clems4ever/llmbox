package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/clems4ever/llmbox/internal/config"
)

// LoginCookie is the name of the cookie holding the opaque login-session ID. The
// value is a random key into the login store; the session data lives server-side.
const LoginCookie = "llmbox_login"

// flowTTL bounds how long an in-flight OIDC handshake may take from redirect to
// callback before its stored state expires.
const flowTTL = 10 * time.Minute

// idClaims are the identity claims llmbox needs from a verified ID token.
type idClaims struct {
	Email         string
	EmailVerified bool
	HostedDomain  string // Google "hd" claim, present for Workspace accounts
}

// idTokenVerifier verifies a raw ID token (and its nonce) and returns the
// identity claims. The real implementation wraps *oidc.IDTokenVerifier; tests
// inject a fake so the HTTP flow can be exercised without a live provider.
type idTokenVerifier interface {
	verify(ctx context.Context, rawIDToken, nonce string) (idClaims, error)
}

// provider is one configured sign-in option (e.g. Google).
type provider struct {
	name           string // URL segment and config key, e.g. "google"
	label          string // human label for the sign-in button, e.g. "Google"
	oauth2         *oauth2.Config
	verifier       idTokenVerifier
	allowedDomains map[string]bool // lower-cased
	allowedEmails  map[string]bool // lower-cased
}

// authorize reports whether the verified claims are allowed to activate boxes.
// The email must be verified, and either explicitly allow-listed or in an
// allowed domain; for Google Workspace accounts the hd claim, when present, must
// also be an allowed domain.
//
// @arg c The verified identity claims.
// @return bool True when the identity may activate boxes.
//
// @testcase TestAuthorize allows allow-listed emails and domains and rejects others.
func (p *provider) authorize(c idClaims) bool {
	if !c.EmailVerified || c.Email == "" {
		return false
	}
	email := strings.ToLower(c.Email)
	if p.allowedEmails[email] {
		return true
	}
	domain := emailDomain(email)
	if domain == "" || !p.allowedDomains[domain] {
		return false
	}
	// A hosted-domain claim, when present, is the authoritative Workspace domain;
	// require it to also be allowed so a Workspace user with an external primary
	// email can't slip through on the email domain alone.
	if c.HostedDomain != "" && !p.allowedDomains[strings.ToLower(c.HostedDomain)] {
		return false
	}
	return true
}

// emailDomain returns the lower-cased domain part of an email address, or "".
//
// @arg email The email address.
// @return string The part after the last "@", lower-cased, or "" if none.
//
// @testcase TestAuthorize relies on domain extraction for domain matching.
func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// Authenticator gates box activation behind provider sign-in. A nil
// *Authenticator means activation is unauthenticated (no provider configured).
type Authenticator struct {
	providers  map[string]*provider
	order      []string // provider names in config order, for stable button order
	sessionTTL time.Duration

	// adminEmails is the lower-cased set of identities allowed into the admin UI.
	// Empty (nil) means the admin UI is disabled. This is independent of any
	// provider's box-activation allow rule.
	adminEmails map[string]bool
}

// providerButton is one sign-in option rendered on the activation page.
type providerButton struct {
	Label     string
	LoginPath string
}

// NewAuthenticator builds an Authenticator from the auth configuration, doing
// OIDC discovery for each enabled provider. It returns (nil, nil) when no
// provider is enabled, which leaves activation unauthenticated.
//
// @arg ctx Context for provider discovery.
// @arg cfg The auth configuration (providers, session TTL).
// @return *Authenticator The authenticator, or nil when no provider is enabled.
// @error error if an enabled provider cannot be discovered.
//
// @testcase TestNewAuthenticatorDisabled returns nil when no provider is enabled.
func NewAuthenticator(ctx context.Context, cfg config.AuthConfig) (*Authenticator, error) {
	a := &Authenticator{
		providers:   map[string]*provider{},
		sessionTTL:  time.Duration(cfg.SessionTTL),
		adminEmails: lowerSet(cfg.Admin.Emails),
	}
	if cfg.Google.Enabled {
		oidcProvider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
		if err != nil {
			return nil, fmt.Errorf("discovering Google OIDC provider: %w", err)
		}
		a.providers["google"] = &provider{
			name:  "google",
			label: "Google",
			oauth2: &oauth2.Config{
				ClientID:     cfg.Google.ClientID,
				ClientSecret: cfg.Google.ClientSecret,
				Endpoint:     oidcProvider.Endpoint(),
				RedirectURL:  cfg.Google.RedirectURL,
				Scopes:       []string{oidc.ScopeOpenID, "email"},
			},
			verifier:       oidcVerifier{oidcProvider.Verifier(&oidc.Config{ClientID: cfg.Google.ClientID})},
			allowedDomains: lowerSet(cfg.Google.AllowedDomains),
			allowedEmails:  lowerSet(cfg.Google.AllowedEmails),
		}
		a.order = append(a.order, "google")
	}
	if len(a.providers) == 0 {
		return nil, nil
	}
	return a, nil
}

// NewTestAuthenticator builds an admin-enabled Authenticator for tests, with a
// single "google" sign-in button (for stable button order) and the given emails
// on the admin allow list. It does no OIDC discovery, so it is usable offline;
// the stub provider it installs cannot complete a real login. This is the seam
// external test packages use to construct an admin Authenticator.
//
// @arg adminEmails The identities allowed into the admin UI; lower-cased here.
// @return *Authenticator An admin-enabled authenticator backed by a stub provider.
//
// @testcase TestAdminDashboardGate exercises an admin-enabled authenticator built here.
func NewTestAuthenticator(adminEmails ...string) *Authenticator {
	return &Authenticator{
		providers:   map[string]*provider{"google": {name: "google", label: "Google"}},
		order:       []string{"google"},
		sessionTTL:  time.Hour,
		adminEmails: lowerSet(adminEmails),
	}
}

// provider returns the configured provider for name.
//
// @arg name The provider URL segment (e.g. "google").
// @return *provider The provider, or nil when not configured.
// @return bool True when a provider matched.
//
// @testcase TestProviderLoginRedirects looks up a provider by name.
func (a *Authenticator) provider(name string) (*provider, bool) {
	if a == nil {
		return nil, false
	}
	p, ok := a.providers[name]
	return p, ok
}

// buttons returns the sign-in buttons to render for an activation page, each
// linking back to the given box token after login.
//
// @arg token The box auth token to return to after sign-in.
// @return []providerButton One button per enabled provider, in config order.
//
// @testcase TestAuthPageRequiresLogin renders the sign-in buttons.
func (a *Authenticator) buttons(token string) []providerButton {
	if a == nil {
		return nil
	}
	out := make([]providerButton, 0, len(a.order))
	for _, name := range a.order {
		out = append(out, providerButton{
			Label:     a.providers[name].label,
			LoginPath: "/auth/" + name + "/login?token=" + url.QueryEscape(token),
		})
	}
	return out
}

// AdminEnabled reports whether the admin UI is enabled (an admin allow-list is
// configured). It is false on a nil Authenticator.
//
// @return bool True when at least one admin email is configured.
//
// @testcase TestAdminAllowlist reports enabled only when emails are configured.
func (a *Authenticator) AdminEnabled() bool {
	return a != nil && len(a.adminEmails) > 0
}

// isAdmin reports whether the (case-insensitive) email is on the admin
// allow-list. It is false on a nil Authenticator or empty list.
//
// @arg email The signed-in identity's email.
// @return bool True when the email may use the admin UI.
//
// @testcase TestAdminAllowlist admits listed emails (any case) and rejects others.
func (a *Authenticator) isAdmin(email string) bool {
	if a == nil || email == "" {
		return false
	}
	return a.adminEmails[strings.ToLower(strings.TrimSpace(email))]
}

// adminButtons returns the sign-in buttons for the admin UI, each returning to
// returnTo (a local path) after sign-in rather than to a box activation page.
//
// @arg returnTo The local path to return to after sign-in (e.g. "/admin").
// @return []providerButton One button per enabled provider, in config order.
//
// @testcase TestAdminButtonsReturnPath builds login links carrying the return path.
func (a *Authenticator) adminButtons(returnTo string) []providerButton {
	if a == nil {
		return nil
	}
	out := make([]providerButton, 0, len(a.order))
	for _, name := range a.order {
		out = append(out, providerButton{
			Label:     a.providers[name].label,
			LoginPath: "/auth/" + name + "/login?return=" + url.QueryEscape(returnTo),
		})
	}
	return out
}

// safeReturnPath returns p when it is a safe local path to redirect to after
// sign-in (an absolute path that is not protocol-relative), or "" otherwise. It
// blocks open redirects: only same-origin paths beginning with a single "/" are
// allowed, and any path with a scheme, host, or backslash is rejected.
//
// @arg p The candidate return path from the login request.
// @return string The path when safe, or "" when it must not be used.
//
// @testcase TestSafeReturnPath accepts local paths and rejects absolute/protocol-relative ones.
func safeReturnPath(p string) string {
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return ""
	}
	if strings.ContainsAny(p, "\\") {
		return ""
	}
	// Reject anything that parses to a non-empty scheme or host (defence in depth).
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return p
}

// oidcVerifier adapts *oidc.IDTokenVerifier to idTokenVerifier, checking the
// nonce and extracting the claims llmbox cares about.
type oidcVerifier struct{ v *oidc.IDTokenVerifier }

// verify validates the raw ID token and its nonce, returning the identity claims.
//
// @arg ctx Context for the verification (JWKS fetch).
// @arg rawIDToken The raw JWT ID token from the token response.
// @arg nonce The nonce that must match the one issued at login start.
// @return idClaims The email, verification flag, and hosted domain.
// @error error if the token is invalid or the nonce does not match.
//
// @testcase TestProviderCallbackActivates uses a fake verifier in place of this.
func (o oidcVerifier) verify(ctx context.Context, rawIDToken, nonce string) (idClaims, error) {
	idt, err := o.v.Verify(ctx, rawIDToken)
	if err != nil {
		return idClaims{}, err
	}
	if idt.Nonce != nonce {
		return idClaims{}, errors.New("oidc nonce mismatch")
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		HD            string `json:"hd"`
	}
	if err := idt.Claims(&c); err != nil {
		return idClaims{}, err
	}
	return idClaims{Email: c.Email, EmailVerified: c.EmailVerified, HostedDomain: c.HD}, nil
}

// handleProviderLogin begins an OIDC handshake: it persists fresh state (PKCE
// verifier + nonce + where to return) and redirects to the provider. The return
// target is either a box activation token (?token=, the activation flow) or a
// safe local path (?return=, the admin flow); exactly one is required.
//
// @arg w The response writer (redirected to the provider).
// @arg r The request carrying {provider} and either a token or return query param.
//
// @testcase TestProviderLoginRedirects redirects to the provider with state.
// @testcase TestProviderLoginReturnPath accepts a safe return path for the admin flow.
func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth.provider(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("token")
	returnTo := safeReturnPath(r.URL.Query().Get("return"))
	if token == "" && returnTo == "" {
		http.Error(w, "missing box token or return path", http.StatusBadRequest)
		return
	}
	state, err1 := randToken(32)
	nonce, err2 := randToken(32)
	if err1 != nil || err2 != nil {
		s.logger().Error("generating login state", "err", errors.Join(err1, err2))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	flow := loginFlow{
		Provider:    p.name,
		ReturnToken: token,
		ReturnTo:    returnTo,
		Nonce:       nonce,
		Verifier:    verifier,
		ExpiresAt:   time.Now().Add(flowTTL),
	}
	if err := s.store.SaveLoginFlow(state, flow); err != nil {
		s.logger().Error("saving login flow", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	authURL := p.oauth2.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleProviderCallback completes an OIDC handshake: it consumes the stored
// flow, exchanges the code, verifies the ID token, and on success creates a login
// session recording the identity's box-activation and admin capabilities, then
// redirects to the flow's return target (a box auth page or the admin UI). It
// rejects only an identity with neither capability.
//
// @arg w The response writer (redirected to the return target on success).
// @arg r The request carrying {provider}, the code, and the state parameter.
//
// @testcase TestProviderCallbackActivates signs in an allowed identity and sets the cookie.
// @testcase TestProviderCallbackRejectsUnauthorized 403s an identity in neither allow rule.
// @testcase TestProviderCallbackAdminOnly signs in an admin who cannot activate boxes.
func (s *Server) handleProviderCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth.provider(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "sign-in was cancelled or failed: "+e, http.StatusBadRequest)
		return
	}
	flow, ok, err := s.store.TakeLoginFlow(q.Get("state"))
	if err != nil {
		s.logger().Error("reading login flow", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok || flow.Provider != p.name || time.Now().After(flow.ExpiresAt) {
		http.Error(w, "your sign-in link expired or was already used; please start again", http.StatusBadRequest)
		return
	}
	tok, err := p.oauth2.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		s.logger().Warn("oidc code exchange failed", "provider", p.name, "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		http.Error(w, "sign-in failed: no id_token", http.StatusBadGateway)
		return
	}
	claims, err := p.verifier.verify(r.Context(), rawID, flow.Nonce)
	if err != nil {
		s.logger().Warn("oidc id token verification failed", "provider", p.name, "err", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	// A sign-in confers two independent capabilities: activating boxes (the
	// provider's allow rule) and using the admin UI (the admin allow-list). The
	// session records both so each surface enforces its own gate. We reject only
	// when the identity has neither capability — an admin who cannot activate
	// boxes (or vice versa) is still allowed to sign in for what they can do.
	canActivate := p.authorize(claims)
	isAdmin := s.auth.isAdmin(claims.Email)
	if !canActivate && !isAdmin {
		s.logger().Info("sign-in denied for unauthorized identity", "provider", p.name, "email", claims.Email)
		http.Error(w, fmt.Sprintf("Signed in as %s, but that account is not authorized here.", claims.Email), http.StatusForbidden)
		return
	}

	id, err1 := randToken(32)
	csrf, err2 := randToken(32)
	if err1 != nil || err2 != nil {
		s.logger().Error("generating login session", "err", errors.Join(err1, err2))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(s.auth.sessionTTL)
	if err := s.store.SaveLoginSession(id, LoginSession{
		Email:     claims.Email,
		Provider:  p.name,
		CSRF:      csrf,
		ExpiresAt: expires,
		Activate:  canActivate,
		Admin:     isAdmin,
	}); err != nil {
		s.logger().Error("saving login session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  LoginCookie,
		Value: id,
		// Scoped to the whole site so the cookie reaches both the activation pages
		// under /auth and the admin UI under /admin.
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	if flow.ReturnTo != "" {
		http.Redirect(w, r, flow.ReturnTo, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/auth/"+url.PathEscape(flow.ReturnToken), http.StatusFound)
}

// currentLogin returns the live, unexpired login session for the request's
// cookie, or false when the visitor is not signed in.
//
// @arg r The incoming request (read for the login cookie).
// @return LoginSession The signed-in session when present and unexpired.
// @return bool True when a valid login session exists.
//
// @testcase TestAuthPageRequiresLogin treats a missing cookie as not-signed-in.
func (s *Server) currentLogin(r *http.Request) (LoginSession, bool) {
	c, err := r.Cookie(LoginCookie)
	if err != nil {
		return LoginSession{}, false
	}
	ls, ok, err := s.store.LoginSession(c.Value)
	if err != nil || !ok || time.Now().After(ls.ExpiresAt) {
		return LoginSession{}, false
	}
	return ls, true
}

// randToken returns a URL-safe random token of nBytes of entropy.
//
// @arg nBytes The number of random bytes to draw.
// @return string The base64url (no padding) encoding of the random bytes.
// @error error if the system random source fails.
//
// @testcase TestProviderLoginRedirects exercises token generation via login.
func randToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// lowerSet builds a set of lower-cased strings from a slice, or nil when empty.
//
// @arg items The strings to lower-case and collect.
// @return map[string]bool A set keyed by the lower-cased items, or nil if none.
//
// @testcase TestAuthorize relies on the lower-cased allow sets.
func lowerSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		if it = strings.TrimSpace(strings.ToLower(it)); it != "" {
			m[it] = true
		}
	}
	return m
}
