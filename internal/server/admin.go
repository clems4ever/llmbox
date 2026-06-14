package server

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/clems4ever/llmbox-mcp/internal/pcap"
)

// webFS holds the embedded admin templates and stylesheet, so the binary is
// self-contained.
//
//go:embed web
var webFS embed.FS

var (
	tmplBoxes = template.Must(template.New("").ParseFS(webFS, "web/layout.html", "web/boxes.html"))
	tmplBox   = template.Must(template.New("").ParseFS(webFS, "web/layout.html", "web/box.html"))
)

// boxRow is one box as shown in the admin UI.
type boxRow struct {
	ID, Hostname, Description, Phase, State, Image, Created string
	HasCapture                                             bool
}

// boxListView is the data for the box list page.
type boxListView struct {
	Boxes     []boxRow
	CaptureOn bool
}

// destRow is one destination row with a human-readable byte count.
type destRow struct {
	Hostname, IP, Proto string
	Port, Packets       int
	Bytes               string
}

// boxDetailView is the data for a single box's traffic page.
type boxDetailView struct {
	Box          boxRow
	CaptureOn    bool
	HasCapture   bool
	Summary      pcap.Summary
	Bytes        string
	Span         string
	Destinations []destRow
}

// requireAdmin guards a handler with HTTP Basic Auth against the admin token.
// When no token is configured the admin pages are disabled (404).
//
// @arg next The handler to protect.
// @return http.HandlerFunc The guarded handler.
//
// @testcase TestAdminAuth blocks unauthenticated access and 404s when disabled.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			http.NotFound(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="llmbox admin", charset="UTF-8"`)
			http.Error(w, "Unauthorized.", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleBoxList renders the authenticated list of all managed boxes.
//
// @arg w The response writer.
// @arg r The request.
//
// @testcase TestAdminPages renders the box list for an authenticated request.
func (s *Server) handleBoxList(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.ListBoxes(r.Context())
	if err != nil {
		http.Error(w, "could not list boxes", http.StatusInternalServerError)
		return
	}
	view := boxListView{CaptureOn: s.captureDir != ""}
	for _, b := range boxes {
		view.Boxes = append(view.Boxes, boxRow{
			ID:          b.ID,
			Hostname:    b.Hostname,
			Description: b.Description,
			Phase:       b.Phase,
			State:       b.State,
			Image:       b.Image,
			Created:     relativeUnix(b.Created),
			HasCapture:  s.captureDir != "" && pcap.Available(s.captureDir, b.ID),
		})
	}
	renderPage(w, tmplBoxes, view)
}

// handleBoxDetail renders one box's traffic metadata, parsing its capture files.
//
// @arg w The response writer.
// @arg r The request (with the box ID path value).
//
// @testcase TestAdminPages renders a box's traffic page including captured destinations.
func (s *Server) handleBoxDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	boxes, err := s.ListBoxes(r.Context())
	if err != nil {
		http.Error(w, "could not list boxes", http.StatusInternalServerError)
		return
	}
	var row *boxRow
	for _, b := range boxes {
		if b.ID == id {
			row = &boxRow{ID: b.ID, Hostname: b.Hostname, Description: b.Description, Phase: b.Phase, State: b.State, Image: b.Image}
			break
		}
	}
	if row == nil {
		http.NotFound(w, r)
		return
	}

	view := boxDetailView{Box: *row, CaptureOn: s.captureDir != ""}
	if view.CaptureOn && pcap.Available(s.captureDir, id) {
		sum, err := pcap.Summarize(s.captureDir, id)
		if err != nil {
			http.Error(w, "could not read capture", http.StatusInternalServerError)
			return
		}
		view.HasCapture = true
		view.Summary = sum
		view.Bytes = humanBytes(sum.Bytes)
		view.Span = humanSpan(sum.FirstSeen, sum.LastSeen)
		for _, d := range sum.Destinations {
			view.Destinations = append(view.Destinations, destRow{
				Hostname: d.Hostname, IP: d.IP, Proto: d.Proto,
				Port: d.Port, Packets: d.Packets, Bytes: humanBytes(d.Bytes),
			})
		}
	}
	renderPage(w, tmplBox, view)
}

// serveStyle serves the embedded stylesheet.
//
// @arg w The response writer.
// @arg _ The request (unused).
//
// @testcase TestAdminPages loads the embedded stylesheet.
func (s *Server) serveStyle(w http.ResponseWriter, _ *http.Request) {
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		http.Error(w, "missing stylesheet", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(css)
}

// renderPage executes a template's "layout" into a buffer, then writes it, so a
// template error can't leave a half-written page.
//
// @arg w The response writer.
// @arg t The parsed template set.
// @arg data The template data.
//
// @testcase TestAdminPages renders pages through renderPage.
func renderPage(w http.ResponseWriter, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// humanBytes formats a byte count as a compact human-readable size.
//
// @arg n The number of bytes.
// @return string A size like "1.5 MB" (or "512 B" below 1 KiB).
//
// @testcase TestHumanBytes formats byte counts across magnitudes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanSpan formats the duration between two capture timestamps.
//
// @arg first The earliest packet time.
// @arg last The latest packet time.
// @return string A short duration like "2m" or "—" when unknown.
//
// @testcase TestHumanBytes also covers humanSpan formatting.
func humanSpan(first, last time.Time) string {
	if first.IsZero() || last.IsZero() || !last.After(first) {
		return "—"
	}
	d := last.Sub(first)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// relativeUnix formats a unix timestamp as a relative age like "5m ago".
//
// @arg sec The unix timestamp in seconds; 0 renders as "—".
// @return string The relative age.
//
// @testcase TestHumanBytes also covers relativeUnix formatting.
func relativeUnix(sec int64) string {
	if sec == 0 {
		return "—"
	}
	d := time.Since(time.Unix(sec, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
