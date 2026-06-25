//go:build e2e

package e2e

import (
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
)

// fakeAnthropic simulates the Claude OAuth ("sign in with Claude") platform that
// an llmbox is activated against. It plays both roles the real platform plays in
// the flow: it serves the browser-facing consent page where the user approves
// access and is shown a one-time code, and it validates that code when the box
// later exchanges it for a remote-control session. It runs entirely in-process
// over httptest, so the workflow needs no network access to claude.com.
type fakeAnthropic struct {
	srv *httptest.Server

	sessionRand io.Reader
	miscRand    io.Reader

	mu     sync.Mutex
	logins map[string]*fakeLogin // keyed by the OAuth "state" parameter
}

// fakeLogin is the platform's record of one in-flight authorization: created
// when a box begins login, completed once the user approves it in the browser.
type fakeLogin struct {
	// code is the one-time code shown to the user after they approve; it is empty
	// until approval. sessionURL is what a valid code exchanges for.
	code       string
	sessionURL string
	approved   bool
}

// newFakeAnthropic starts the simulated platform and returns it ready to use.
//
// @return *fakeAnthropic A running simulated Anthropic OAuth platform.
func newFakeAnthropic() *fakeAnthropic {
	a := &fakeAnthropic{
		logins:      map[string]*fakeLogin{},
		sessionRand: rand.New(rand.NewSource(0)),
		miscRand:    rand.New(rand.NewSource(100)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize", a.handleAuthorize)
	mux.HandleFunc("POST /oauth/approve", a.handleApprove)
	a.srv = httptest.NewServer(mux)
	return a
}

// close shuts down the platform's HTTP server.
func (a *fakeAnthropic) close() { a.srv.Close() }

// beginLogin records a new in-flight authorization and returns its OAuth state
// plus the browser-facing authorize URL the box would print. The box keeps the
// state to exchange the code later; the user's browser opens the URL.
//
// @return state The opaque OAuth state identifying this authorization.
// @return authorizeURL The consent-page URL the user opens to approve access.
func (a *fakeAnthropic) beginLogin() (state, authorizeURL string) {
	state = randHex(a.miscRand, 16)
	a.mu.Lock()
	a.logins[state] = &fakeLogin{}
	a.mu.Unlock()
	// A realistic-looking authorize URL (PKCE challenge + state), but pointed at
	// this in-process platform instead of claude.com so the browser can reach it.
	authorizeURL = fmt.Sprintf(
		"%s/oauth/authorize?response_type=code&code_challenge=%s&code_challenge_method=S256&state=%s",
		a.srv.URL, randHex(a.miscRand, 16), state,
	)
	return state, authorizeURL
}

// exchange validates a code against an authorization and returns the session URL
// the box should surface. It mirrors the real out-of-band code exchange: a code
// is only valid once the user has approved the matching authorization.
//
// @arg state The OAuth state the box began login with.
// @arg code The code the user pasted into the activation page.
// @return string The remote-control session URL on success.
// @error error if the state is unknown, unapproved, or the code does not match.
func (a *fakeAnthropic) exchange(state, code string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	login, ok := a.logins[state]
	if !ok {
		return "", fmt.Errorf("unknown authorization state")
	}
	if !login.approved || code == "" || code != login.code {
		return "", fmt.Errorf("invalid or expired code")
	}
	return login.sessionURL, nil
}

// handleAuthorize renders the consent page for a known authorization state,
// showing an "Approve access" button before approval and the one-time code
// after.
func (a *fakeAnthropic) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	a.mu.Lock()
	login, ok := a.logins[state]
	var data consentData
	if ok {
		data = consentData{State: state, Approved: login.approved, Code: login.code}
	}
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown authorization state", http.StatusBadRequest)
		return
	}
	renderConsent(w, data)
}

// handleApprove marks an authorization approved, mints its one-time code and
// session URL, and re-renders the consent page showing that code.
func (a *fakeAnthropic) handleApprove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	state := r.PostFormValue("state")
	a.mu.Lock()
	login, ok := a.logins[state]
	if ok && !login.approved {
		login.approved = true
		login.code = randHex(a.miscRand, 12)
		// A fixed session URL keeps the activated ("ready") screenshot byte-stable
		// across runs; a random suffix would otherwise rewrite auth-ready.png (and
		// its CI preview comment) on every PR. The code above stays random because
		// it never appears in a screenshot.
		login.sessionURL = "https://claude.ai/code/sessions/" + randHex(a.sessionRand, 8)
	}
	var data consentData
	if ok {
		data = consentData{State: state, Approved: true, Code: login.code}
	}
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown authorization state", http.StatusBadRequest)
		return
	}
	renderConsent(w, data)
}

// consentData is the view model for the simulated platform's consent page.
type consentData struct {
	State    string
	Approved bool
	Code     string
}

// renderConsent writes the consent page for the given state.
func renderConsent(w http.ResponseWriter, data consentData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTmpl.Execute(w, data)
}

// consentTmpl is the simulated platform's single consent page: it shows an
// approve button before approval and the one-time code afterwards. The element
// IDs (#approve, #oauth-code) are the stable selectors the WebDriver test drives.
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Sign in with Claude</title></head>
<body>
  <h1 id="consent-title">Sign in with Claude</h1>
{{if .Approved}}
  <p id="approved">Access approved. Copy this code and paste it back into llmbox:</p>
  <code id="oauth-code">{{.Code}}</code>
{{else}}
  <p id="account">Signed in as e2e-user@example.com</p>
  <form method="post" action="/oauth/approve">
    <input type="hidden" name="state" value="{{.State}}">
    <button id="approve" type="submit">Approve access</button>
  </form>
{{end}}
</body></html>`))

// randHex returns n random bytes hex-encoded, for stand-in tokens and codes.
//
// @arg n The number of random bytes to generate.
// @return string The hex encoding of n random bytes.
func randHex(r io.Reader, n int) string {
	b := make([]byte, n)
	if _, err := r.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
