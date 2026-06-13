package server

import (
	"html/template"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler builds the HTTP handler serving both the MCP endpoint (at /mcp) and
// the auth web pages (at /auth/{token}). mcpServer is reused across sessions.
func (s *Server) Handler(mcpServer *mcp.Server) http.Handler {
	mux := http.NewServeMux()

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)

	mux.HandleFunc("GET /auth/{token}", s.handleAuthPage)
	mux.HandleFunc("POST /auth/{token}", s.handleAuthSubmit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

// handleAuthPage renders the current state of an auth session.
func (s *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	sess := s.lookup(r.PathValue("token"))
	if sess == nil {
		http.Error(w, "Unknown or expired authentication session.", http.StatusNotFound)
		return
	}
	status, sessionURL, errMsg := sess.snapshot()
	render(w, authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
	})
}

// handleAuthSubmit feeds the pasted code to the box, then re-renders the page.
func (s *Server) handleAuthSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sess := s.lookup(token)
	if sess == nil {
		http.Error(w, "Unknown or expired authentication session.", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	// SubmitCode blocks until login completes (or fails); it records the result
	// on the session, which we then render. The code itself is never logged.
	_ = s.SubmitCode(r.Context(), token, r.PostFormValue("code"))

	status, sessionURL, errMsg := sess.snapshot()
	render(w, authPageData{
		Token:        sess.Token,
		AuthorizeURL: template.URL(sess.AuthorizeURL),
		Status:       status,
		SessionURL:   sessionURL,
		Error:        errMsg,
	})
}

type authPageData struct {
	Token        string
	AuthorizeURL template.URL
	Status       string
	SessionURL   string
	Error        string
}

func render(w http.ResponseWriter, data authPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Don't let intermediaries cache an auth page (it contains live state).
	w.Header().Set("Cache-Control", "no-store")
	if data.Status == "ready" {
		w.WriteHeader(http.StatusOK)
	}
	_ = authTmpl.Execute(w, data)
}

var authTmpl = template.Must(template.New("auth").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authenticate your llmbox</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 640px; margin: 3rem auto; padding: 0 1rem; line-height: 1.5; }
  .step { margin: 1.5rem 0; }
  code { background: #f2f2f2; padding: .1rem .3rem; border-radius: 3px; }
  input[type=text] { width: 100%; padding: .6rem; font-size: 1rem; box-sizing: border-box; }
  button { margin-top: .8rem; padding: .6rem 1.2rem; font-size: 1rem; cursor: pointer; }
  .btn-link { display: inline-block; padding: .6rem 1.2rem; background: #d97757; color: #fff; text-decoration: none; border-radius: 6px; }
  .ok { color: #1a7f37; } .err { color: #b42318; }
  .url { word-break: break-all; }
</style>
</head>
<body>
<h1>Authenticate your llmbox</h1>

{{if eq .Status "ready"}}
  <p class="ok"><strong>Your llmbox is ready.</strong></p>
  {{if .SessionURL}}
    <p>Drive it from here:</p>
    <p class="url"><a href="{{.SessionURL}}">{{.SessionURL}}</a></p>
  {{else}}
    <p>Authentication succeeded.</p>
  {{end}}
{{else}}
  {{if eq .Status "error"}}
    <p class="err"><strong>That didn't work:</strong> {{.Error}}</p>
    <p>The code may have been mistyped or expired. Get a fresh code and try again.</p>
  {{end}}

  <div class="step">
    <h2>Step 1 — Sign in</h2>
    <p>Open the Claude sign-in page, approve access, then copy the code it shows you.</p>
    <p><a class="btn-link" href="{{.AuthorizeURL}}" target="_blank" rel="noopener noreferrer">Sign in with Claude</a></p>
  </div>

  <div class="step">
    <h2>Step 2 — Paste the code</h2>
    <form method="post" action="/auth/{{.Token}}" autocomplete="off">
      <input type="text" name="code" placeholder="Paste your code here" autofocus
             autocapitalize="off" autocorrect="off" spellcheck="false">
      <br>
      <button type="submit">Activate llmbox</button>
    </form>
    <p><small>Your code goes straight to this server and into your private sandbox — it is never sent to the chatbot.</small></p>
  </div>
{{end}}
</body>
</html>`))
