package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// sessionWatcherScript builds the <script> injected into proxied HTML documents.
// The script polls the proxy's same-origin auth-check endpoint on an interval and,
// as soon as it sees the session is gone (a 401), navigates the top-level window
// to the public sign-in page carrying the current page as the return target — so a
// signed-in user sitting on a proxied single-page app lands on sign-in shortly
// after the session expires, instead of the app's background requests failing
// silently and it "appearing disconnected". A same-origin poll is also fired when
// the tab regains focus, so switching back to a long-idle tab redirects at once.
//
// The interval and the sign-in URL are server-computed and embedded via
// json.Marshal, whose default HTML escaping (< for '<') prevents any value
// from breaking out of the <script> element.
//
// @arg r The document request (its Host and URI form the sign-in return target).
// @return string A self-contained <script>…</script> element.
//
// @testcase TestProxyInjectsSessionWatcher injects a watcher carrying the sign-in URL and check path.
func (s *Server) sessionWatcherScript(r *http.Request) string {
	interval := s.proxyAuthCheckInterval
	if interval <= 0 {
		interval = defaultProxyAuthCheckInterval
	}
	signIn, _ := json.Marshal(s.signInURL(r))
	checkPath, _ := json.Marshal(proxyAuthCheckPath)
	intervalMS, _ := json.Marshal(interval.Milliseconds())
	// A small, dependency-free watcher. It never touches the app's own requests; it
	// only polls the reserved auth-check endpoint, so a legitimate 401 from the app
	// cannot trigger a spurious redirect.
	return fmt.Sprintf(`<script>(function(){
try{
var signInURL=%s,checkPath=%s,intervalMS=%s,redirecting=false;
function toSignIn(){if(redirecting)return;redirecting=true;window.location.href=signInURL;}
function check(){fetch(checkPath,{credentials:"same-origin",cache:"no-store",headers:{"X-Requested-With":"llmbox-session-watcher"}}).then(function(resp){if(resp.status===401)toSignIn();}).catch(function(){});}
setInterval(check,intervalMS);
window.addEventListener("focus",check);
document.addEventListener("visibilitychange",function(){if(!document.hidden)check();});
}catch(e){}
})();</script>`, signIn, checkPath, intervalMS)
}

// injectSessionWatcher splices the session-watcher script into an HTML document
// response body. It is a no-op for anything that is not an uncompressed HTML
// document served with a success status, so proxied APIs, sub-resources, redirects,
// and error pages are passed through untouched. When it does inject, it buffers the
// (typically small) document, inserts the script just before </head> — falling back
// to before </body>, then to the start — and fixes Content-Length.
//
// @arg resp The upstream response to (maybe) rewrite in place.
// @arg script The <script> element to inject.
// @error error if the response body cannot be read.
//
// @testcase TestProxyInjectsSessionWatcher injects into an HTML document body.
// @testcase TestProxyInjectSkipsNonHTML leaves a non-HTML response body unchanged.
func injectSessionWatcher(resp *http.Response, script string) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), "text/html") {
		return nil
	}
	// Only splice into a plaintext body: the outbound request dropped Accept-Encoding
	// so a compliant box returns the document uncompressed, but a box that compresses
	// anyway is left untouched rather than corrupted.
	if resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	injected := insertBeforeTag(body, []byte(script))
	resp.Body = io.NopCloser(bytes.NewReader(injected))
	resp.ContentLength = int64(len(injected))
	resp.Header.Set("Content-Length", strconv.Itoa(len(injected)))
	return nil
}

// insertBeforeTag returns body with script inserted just before the first </head>
// (case-insensitively), or before the first </body> when there is no head, or at
// the very start when the document has neither closing tag — so the watcher runs
// on any HTML shape without depending on a well-formed document.
//
// @arg body The HTML document bytes.
// @arg script The bytes to insert.
// @return []byte A new slice with script spliced in.
//
// @testcase TestInsertBeforeTag inserts before </head>, falls back to </body>, then prepends.
func insertBeforeTag(body, script []byte) []byte {
	lower := bytes.ToLower(body)
	at := bytes.Index(lower, []byte("</head>"))
	if at < 0 {
		at = bytes.Index(lower, []byte("</body>"))
	}
	if at < 0 {
		at = 0
	}
	out := make([]byte, 0, len(body)+len(script))
	out = append(out, body[:at]...)
	out = append(out, script...)
	out = append(out, body[at:]...)
	return out
}
