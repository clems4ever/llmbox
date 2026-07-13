package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/llmbox/internal/hub/auth"
	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/testutils"
)

// TestProxyInjectsSessionWatcher checks that, with auth on, an HTML document
// forwarded through the proxy to a signed-in browser carries the injected session
// watcher: the auth-check path it polls and the sign-in URL it redirects to. This
// is the client half of the fix — a single-page app that never navigates still
// learns its session expired and sends the user to sign-in.
func TestProxyInjectsSessionWatcher(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title>app</title></head><body>hi</body></html>"))
	}))
	defer upstream.Close()

	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, st := newProxyServer(t, mgr, a)
	s.SetProxyAuthCheckInterval(2 * time.Second)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutIdentitySession(hashTok("SID"), store.IdentitySession{Email: "dev@corp.com", CanAdmin: true, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: auth.LoginCookie, Value: "SID"})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "<body>hi</body>") {
		t.Errorf("original document not preserved: %q", got)
	}
	if !strings.Contains(got, proxyAuthCheckPath) {
		t.Errorf("watcher does not reference the auth-check path %q: %q", proxyAuthCheckPath, got)
	}
	if !strings.Contains(got, "/signin?return=") {
		t.Errorf("watcher does not carry a sign-in URL: %q", got)
	}
	if !strings.Contains(got, "2000") { // the 2s interval, in ms
		t.Errorf("watcher does not carry the configured poll interval: %q", got)
	}
	// The script must land inside the head, before the app's content runs.
	if strings.Index(got, "<script>") > strings.Index(got, "</head>") {
		t.Errorf("watcher not injected before </head>: %q", got)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl == "59" {
		t.Errorf("Content-Length not updated after injection (still the original 59): %q", cl)
	}
}

// TestProxyAuthCheckEndpoint checks the reserved auth-check path reports the live
// session state without ever forwarding to the box: 204 for a signed-in admin,
// 401 once the session is gone (the signal the injected watcher redirects on).
func TestProxyAuthCheckEndpoint(t *testing.T) {
	// A box server that must never be reached by the auth-check request.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("auth-check request was forwarded to the box")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, st := newProxyServer(t, mgr, a)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutIdentitySession(hashTok("SID"), store.IdentitySession{Email: "dev@corp.com", CanAdmin: true, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	check := func(cookie *http.Cookie) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+proxyAuthCheckPath, nil)
		req.Host = rec.Slug + ".proxy.example.com"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := check(&http.Cookie{Name: auth.LoginCookie, Value: "SID"}); code != http.StatusNoContent {
		t.Errorf("signed-in auth-check = %d, want 204", code)
	}
	// Expire the session; the endpoint must now report 401.
	if err := st.PutIdentitySession(hashTok("SID"), store.IdentitySession{Email: "dev@corp.com", CanAdmin: true, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if code := check(&http.Cookie{Name: auth.LoginCookie, Value: "SID"}); code != http.StatusUnauthorized {
		t.Errorf("expired auth-check = %d, want 401", code)
	}
	if code := check(nil); code != http.StatusUnauthorized {
		t.Errorf("signed-out auth-check = %d, want 401", code)
	}
}

// TestProxyInjectSkipsNonHTML checks a non-HTML proxied response (an API/JSON
// payload) is passed through byte-for-byte, so the watcher injection never
// corrupts XHR/API traffic the app depends on.
func TestProxyInjectSkipsNonHTML(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	a := auth.NewTestAuthenticator("admin@corp.com")
	mgr := &dialMgr{FakeMgr: &testutils.FakeMgr{CreateID: "abcdef0123456789"}, target: upstream.Listener.Addr().String()}
	s, st := newProxyServer(t, mgr, a)
	registerBox(t, s, "web-box", "")
	rec, err := s.createProxy("web-box", 8000, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutIdentitySession(hashTok("SID"), store.IdentitySession{Email: "dev@corp.com", CanAdmin: true, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.APIHandler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api", nil)
	req.Host = rec.Slug + ".proxy.example.com"
	req.Header.Set("Accept", "text/html") // even a document-shaped request: content-type gates injection
	req.AddCookie(&http.Cookie{Name: auth.LoginCookie, Value: "SID"})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("JSON body altered by injection: %q", string(body))
	}
}

// TestInsertBeforeTag checks the watcher lands before </head>, falls back to
// </body> without a head, and prepends when the document has neither.
func TestInsertBeforeTag(t *testing.T) {
	script := []byte("<X>")
	cases := []struct {
		name, in, want string
	}{
		{"head", "<head></head><body>b</body>", "<head><X></head><body>b</body>"},
		{"headMixedCase", "<HEAD></HEAD>", "<HEAD><X></HEAD>"},
		{"bodyOnly", "<body>b</body>", "<body>b<X></body>"},
		{"neither", "<p>hi</p>", "<X><p>hi</p>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(insertBeforeTag([]byte(c.in), script)); got != c.want {
				t.Errorf("insertBeforeTag(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
