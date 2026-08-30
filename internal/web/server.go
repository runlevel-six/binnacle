// Package web serves the fleet page.
//
// Rendering is server-side and the browser gets no application logic: the
// server holds the verdicts sextant produced, renders them to HTML, and pushes
// a new fragment over Server-Sent Events whenever the fleet moves. There is no
// client-side model to fall out of step with the one on the server, and a
// reader watching an upgrade never has to wonder whether the page is stale.
package web

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/runlevel-six/binnacle/internal/fleet"
)

//go:embed templates/*.html
var templateFS embed.FS

// Authenticator decides whether a request may see the fleet.
//
// It is an interface so that the local development path and the deployed path
// differ in one wiring line rather than in scattered conditionals — and so that
// the deployed path cannot be reached by forgetting to set a flag.
type Authenticator interface {
	// Middleware wraps a handler, rejecting or redirecting unauthenticated
	// requests.
	Middleware(http.Handler) http.Handler
	// Routes registers any endpoints the flow itself needs, such as a callback.
	Routes(*http.ServeMux)
	// Describe names the scheme, for the startup banner and the footer.
	Describe() string
}

// Source is what the server renders: the current state of the fleet, and a
// signal that it may have moved.
//
// It is an interface so the server can be exercised without a cluster. A fleet
// page is mostly a question of what it says when things are wrong, and the
// wrong states — unreachable, unreadable, not yet reported — are exactly the
// ones a live cluster will not produce on demand.
type Source interface {
	// View returns the current state of every cluster, worst first.
	View() []fleet.ClusterView
	// Changed ticks when something may have moved.
	Changed() <-chan struct{}
}

// Server renders the fleet.
type Server struct {
	fleet Source
	auth  Authenticator
	tmpl  *template.Template
	// version is binnacle's own build, shown in the footer so a reader can say
	// which binnacle produced a screenshot.
	version string
}

// New builds the server. It does not listen; use [Server.Handler].
func New(f Source, auth Authenticator, version string) (*Server, error) {
	tmpl, err := template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{fleet: f, auth: auth, tmpl: tmpl, version: version}, nil
}

// Handler returns the routed handler.
//
// Only the fleet routes sit behind authentication. /healthz does not, because a
// readiness probe has no session and blocking it would make the deployment
// unschedulable — and it reveals nothing but whether the process is up.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	s.auth.Routes(mux)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", s.handleFleet)
	protected.HandleFunc("GET /events", s.handleEvents)
	mux.Handle("/", s.auth.Middleware(protected))
	return mux
}

type pageData struct {
	Clusters []fleet.ClusterView
	Auth     string
	Version  string
	Now      time.Time
}

func (s *Server) page() pageData {
	return pageData{
		Clusters: s.fleet.View(),
		Auth:     s.auth.Describe(),
		Version:  s.version,
		Now:      time.Now(),
	}
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "fleet.html", s.page()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleEvents streams a re-rendered cluster grid whenever the fleet changes.
//
// The first frame is sent immediately rather than on the first change: a
// browser that reconnects after a network blip would otherwise sit on whatever
// it had until something in the fleet happened to move, which on a quiet
// morning could be a very long time.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		var buf strings.Builder
		if err := s.tmpl.ExecuteTemplate(&buf, "grid.html", s.page()); err != nil {
			return false
		}
		fmt.Fprint(w, "event: fleet\n")
		// SSE frames are line-oriented, so every line of the fragment needs its
		// own data: prefix.
		sc := bufio.NewScanner(strings.NewReader(buf.String()))
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			fmt.Fprintf(w, "data: %s\n", sc.Text())
		}
		fmt.Fprint(w, "\n")
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}

	// A keepalive keeps an idle stream from being reaped by a proxy between the
	// browser and here — with a fleet that can legitimately be silent for
	// hours, the connection outlives the news.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.fleet.Changed():
			if !send() {
				return
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func funcs() template.FuncMap {
	return template.FuncMap{
		// ago renders a timestamp as coarse relative time. A fleet page reports
		// how long ago something was seen, not when: "4m ago" answers the
		// question a reader actually has, and a wall-clock time makes them do
		// the subtraction themselves.
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t).Round(time.Second)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			default:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			}
		},
		"pct": func(ready, desired int32) int {
			if desired <= 0 {
				return 0
			}
			return int(float64(ready) / float64(desired) * 100)
		},
	}
}

// ServeContext runs the HTTP server until ctx is canceled.
func (s *Server) ServeContext(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// The SSE stream is deliberately long-lived, so there is no write
		// timeout. Read timeouts still apply: a slow request header is not the
		// same thing as a long-lived response.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
