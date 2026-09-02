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
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"

	"github.com/runlevel-six/binnacle/internal/fleet"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS holds the brand marks, cut from the source artwork in assets/.
//
//go:embed static/*.png
var staticFS embed.FS

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
	// Warning is text the page shows across the top, or empty when the scheme
	// needs no warning. A page anyone can read should say so on itself, not
	// only in the flags that started it.
	Warning() string
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
	// Cluster returns everything known about one cluster. False means no such
	// cluster is tracked, which is a 404 rather than an empty cluster: "this
	// cluster has no nodes" and "there is no such cluster" are different
	// claims and must not render the same.
	Cluster(namespace, name string) (fleet.ClusterDetail, bool)
	// Storage returns the datacenter's storage layer, which belongs to no
	// cluster and so cannot be reached through View.
	Storage() fleet.Storage
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
	// site names the management cluster this binnacle watches.
	//
	// One binnacle runs per management cluster, and sites reuse workload
	// cluster names, so several deployments render pages that are identical
	// down to the cluster names on the cards. Without this the only thing
	// telling two open tabs apart is the address bar. It is optional because a
	// single local instance has nothing to be confused with.
	site string
}

// New builds the server. It does not listen; use [Server.Handler].
func New(f Source, auth Authenticator, version, site string) (*Server, error) {
	tmpl, err := template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{fleet: f, auth: auth, tmpl: tmpl, version: version, site: site}, nil
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

	// The marks sit outside authentication. They are a logo, they reveal
	// nothing, and the sign-in page wants them too.
	mux.Handle("GET /static/", http.StripPrefix("/", cacheFor(24*time.Hour, http.FileServer(http.FS(staticFS)))))
	s.auth.Routes(mux)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", s.handleFleet)
	protected.HandleFunc("GET /events", s.handleEvents)
	protected.HandleFunc("GET /cluster/{namespace}/{name}", s.handleCluster)
	protected.HandleFunc("GET /cluster/{namespace}/{name}/events", s.handleClusterEvents)
	protected.HandleFunc("GET /api/v1/fleet", s.handleAPIFleet)
	protected.HandleFunc("GET /api/v1/clusters/{namespace}/{name}", s.handleAPICluster)
	protected.HandleFunc("GET /api/v1/storage", s.handleAPIStorage)
	protected.HandleFunc("GET /api/v1/events", s.handleAPIEvents)
	mux.Handle("/", s.auth.Middleware(protected))
	return mux
}

type pageData struct {
	Clusters []fleet.ClusterView
	// Storage is the datacenter's Ceph hardware. Its own field rather than a
	// cluster's, because it is not one.
	Storage fleet.Storage
	Auth    string
	Warning string
	Version string
	Site    string
	Now     time.Time
	// Wall scales the page up for a display read from across a room. Opt-in
	// through ?display=wall rather than inferred: a 1920-pixel viewport is as
	// likely to be a laptop at arm's length as a television at ten feet, and no
	// media query can tell those apart. A wall display is also set up once and
	// left for months, which is why this is in the URL — it survives a browser
	// restart and a kiosk relaunch, where a stored preference may not.
	Wall bool
}

type clusterData struct {
	Cluster fleet.ClusterDetail
	Auth    string
	Warning string
	Version string
	Site    string
}

func (s *Server) page() pageData {
	return pageData{
		Clusters: s.fleet.View(),
		Storage:  s.fleet.Storage(),
		Auth:     s.auth.Describe(),
		Warning:  s.auth.Warning(),
		Version:  s.version,
		Site:     s.site,
		Now:      time.Now(),
	}
}

func (s *Server) cluster(r *http.Request) (clusterData, bool) {
	d, ok := s.fleet.Cluster(r.PathValue("namespace"), r.PathValue("name"))
	if !ok {
		return clusterData{}, false
	}
	return clusterData{
		Cluster: d, Auth: s.auth.Describe(), Warning: s.auth.Warning(),
		Version: s.version, Site: s.site,
	}, true
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	data, ok := s.cluster(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, "cluster.html", data)
}

// render writes a template, or an error, but never half of each.
//
// html/template streams as it executes, so a failure partway down a page has
// already sent a 200 and most of the markup. The reader then gets a truncated
// page that looks like a rendering quirk rather than a fault, and the error
// text lands in the middle of the document. Buffering costs one page of memory
// and makes the failure honest.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// handleClusterEvents streams the cluster page's body, the same way the fleet
// page streams its grid.
//
// A cluster page is where somebody sits during an upgrade, watching machines
// move through phases. That is precisely when a page that needs reloading is
// worst: the reader cannot tell a quiet cluster from a stale tab.
func (s *Server) handleClusterEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.cluster(r); !ok {
		http.NotFound(w, r)
		return
	}
	s.stream(w, r, "cluster", func() (any, bool) {
		data, ok := s.cluster(r)
		return data, ok
	})
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	d := s.page()
	d.Wall = r.URL.Query().Get("display") == "wall"
	s.render(w, "fleet.html", d)
}

// handleEvents streams a re-rendered cluster grid whenever the fleet changes.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, "fleet", func() (any, bool) { return s.page(), true })
}

// stream pushes a re-rendered fragment over Server-Sent Events.
//
// The first frame goes out immediately rather than on the first change: a
// browser that reconnects after a network blip would otherwise sit on whatever
// it had until something happened to move, which on a quiet morning could be a
// very long time.
//
// fragment names both the template and the SSE event, which keeps the two from
// drifting apart — the browser listens for the name of the thing it is being
// sent.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, fragment string, data func() (any, bool)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		d, ok := data()
		if !ok {
			// The cluster went away while somebody was watching it. Ending the
			// stream is the honest move: the browser stops updating and the
			// reader's next navigation gets the 404.
			return false
		}
		var buf strings.Builder
		if err := s.tmpl.ExecuteTemplate(&buf, fragment+"-body.html", d); err != nil {
			return false
		}
		fmt.Fprintf(w, "event: %s\n", fragment)
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

// cacheFor lets a browser keep an asset rather than re-fetching it on every
// navigation. The marks change only when the brand does.
func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
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
		// age renders a model duration the way the tables do: one unit.
		"age": fleet.Compact,
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		// Migration status vocabulary is OpenStack's and its abbreviations are
		// sextant's. Both come from the same place the dashboard reads them,
		// so the two cannot describe the same migration differently.
		"shortType":   openstack.ShortType,
		"shortStatus": openstack.ShortStatus,
		"active":      openstack.Active,
		"failed":      openstack.Failed,
		// shortID trims an OpenStack UUID to the eight characters that identify
		// it, matching what the dashboard's migration table shows. Local rather
		// than shared from sextant: unlike the health verdicts, how many
		// characters of a UUID to print carries no judgement about whether
		// anything is wrong, so the two front ends drifting on it would cost
		// nothing.
		"shortID": func(s string) string {
			if len(s) > 8 {
				return s[:8]
			}
			return s
		},
		// A map has no order, and a template iterating one sorts by key —
		// which put ACTIVE ahead of ERROR and buried the failing state in the
		// middle of the line. StateCounts orders by count and says which states
		// are failures, so the pane and this page cannot disagree about either.
		"states": openstack.StateCounts,
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
