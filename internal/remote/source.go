// Package remote is an HTTP client for binnacle's JSON API. It implements
// [web.Source], so a sextant running with --server reads the same interface
// the server itself reads — the only difference is where the data came from.
//
// The source polls on demand for View, Cluster and Storage, and subscribes to
// the SSE stream for Changed. A tick on Changed means "something may have
// moved"; the caller re-reads View to find out what.
package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/wire"
)

// TokenFunc supplies the credential to present, and is called for every
// request rather than once.
//
// It is a function because a session outlives its token. An operator leaves
// sextant open for hours; the ID token it signs in with is good for minutes,
// and something has to exchange one for the next. Reading the value per
// request means a renewal reaches the SSE stream's next reconnect too, with no
// plumbing between them.
//
// Nil, or a function returning "", sends no credential — which is what a
// server running with --allow-unauthenticated wants.
type TokenFunc func() string

// StaticToken is a TokenFunc for a credential that never changes: one supplied
// on the command line, or none at all.
func StaticToken(token string) TokenFunc {
	return func() string { return token }
}

// Source is a [web.Source] backed by a binnacle server's JSON API.
type Source struct {
	base    string
	client  *http.Client
	token   TokenFunc
	changed chan struct{}

	// mu guards problem, which the UI reads from its own goroutine.
	mu sync.Mutex
	// problem is why the last fleet read failed, or empty when it worked.
	//
	// Without it an unreachable server and an empty fleet are the same
	// picture: View returns no clusters either way, and the screen says the
	// fleet is fine when nobody has heard from it. That is the distinction
	// NodesKnown draws one layer down, drawn again here.
	problem string
}

// New builds a Source pointing at base, which is the binnacle server's root
// URL (e.g. "http://binnacle:8080").
//
// token supplies the bearer credential for each request; see [TokenFunc].
func New(base string, token TokenFunc) *Source {
	return &Source{
		base:    strings.TrimRight(base, "/"),
		client:  &http.Client{},
		token:   token,
		changed: make(chan struct{}, 1),
	}
}

// authorize stamps the current credential on a request, if there is one.
func (s *Source) authorize(req *http.Request) {
	if s.token == nil {
		return
	}
	if tok := s.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// Run subscribes to the server's SSE stream and blocks until ctx is canceled.
//
// On a connection failure it reconnects after a brief delay, so a transient
// network blip does not silence the stream permanently. Each received event
// sends a non-blocking tick on Changed.
func (s *Source) Run(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.subscribe(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Recorded, not just retried. A stream that is being refused looks
			// exactly like a stream with nothing to say — both leave the screen
			// waiting — and the difference is the whole of what an operator
			// needs to know. The retry still happens; it is no longer silent.
			s.setProblem(fmt.Sprintf("cannot subscribe to %s: %v", s.base, err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
	}
}

// View returns the current fleet view, or nil if the server could not be
// read. When it returns nil, [Source.Problem] says why.
func (s *Source) View() []fleet.ClusterView {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, "/api/v1/fleet")
	if err != nil {
		s.setProblem(fmt.Sprintf("cannot reach %s: %v", s.base, err))
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		s.setProblem(describeStatus(s.base, resp.StatusCode))
		return nil
	}

	var fr struct {
		Clusters []fleet.ClusterView `json:"clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		s.setProblem(fmt.Sprintf("unreadable reply from %s: %v", s.base, err))
		return nil
	}
	s.setProblem("")
	return fr.Clusters
}

// Problem reports why the fleet could not be read, or empty when it could.
func (s *Source) Problem() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.problem
}

func (s *Source) setProblem(p string) {
	s.mu.Lock()
	s.problem = p
	s.mu.Unlock()
}

// describeStatus turns a status code into something an operator can act on.
//
// 401 gets named specifically because it is the one with an obvious remedy and
// the one most likely to happen mid-session: tokens expire, and "unauthorized"
// on its own does not tell anyone to sign in again.
func describeStatus(base string, code int) string {
	switch code {
	case http.StatusUnauthorized:
		return "not signed in to " + base + " — the token may have expired; restart sextant to sign in again"
	case http.StatusForbidden:
		return "not allowed to read the fleet on " + base
	default:
		return fmt.Sprintf("%s returned %s", base, http.StatusText(code))
	}
}

// Cluster returns one cluster's detail. False means the server does not track
// such a cluster, which is a 404 rather than an empty cluster.
func (s *Source) Cluster(namespace, name string) (fleet.ClusterDetail, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, fmt.Sprintf("/api/v1/clusters/%s/%s", namespace, name))
	if err != nil {
		return fleet.ClusterDetail{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fleet.ClusterDetail{}, false
	}
	var d fleet.ClusterDetail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return fleet.ClusterDetail{}, false
	}
	return d, true
}

// Storage returns the datacenter storage layer, or an empty Storage if the
// server could not be reached.
func (s *Source) Storage() fleet.Storage {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, "/api/v1/storage")
	if err != nil {
		return fleet.Storage{}
	}
	defer func() { _ = resp.Body.Close() }()
	var st fleet.Storage
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return fleet.Storage{}
	}
	return st
}

// Management returns an empty ManagementView, because the API does not carry
// the management cluster yet.
//
// **Not a boundary — an unfinished feature.** Nothing about the design keeps it
// out. The data is already collected, shaped and rendered; the web page serves
// it at /management. It is simply not prioritized, and until it is the two front
// ends are not at parity.
//
// What closing it takes, so the next person does not have to work it out: a
// Management field on the server's fleetResponse (or its own route, as Storage
// has), a real implementation here that reads it, the method added to the
// fleetSource interface in internal/ui — which deliberately lists only what the
// fleet screen needs — and somewhere in the terminal UI to put it. That last
// part is the actual work: the fleet screen is one line per workload cluster,
// and the management cluster is a different shape rather than another row.
//
// Returning a zero value is safe in the meantime. Reachable is false and
// ErrText is empty, which every renderer treats as "no management data at all"
// and omits the section — rather than drawing an empty one that would read as a
// healthy management cluster.
func (s *Source) Management() fleet.ManagementView {
	return fleet.ManagementView{}
}

// StoreSnapshot fetches one cluster's raw store contents from the server's
// stream endpoint. It is a synchronous poll used by ClusterSource.Detect;
// the streaming path uses the SSE endpoint directly.
func (s *Source) StoreSnapshot(namespace, name string) ([]wire.Entry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, fmt.Sprintf("/api/v1/clusters/%s/%s/snapshot", namespace, name))
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false
	}
	var entries []wire.Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, false
	}
	return entries, true
}

// Changed returns a channel that ticks when the fleet may have moved.
func (s *Source) Changed() <-chan struct{} { return s.changed }

func (s *Source) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Source) subscribe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	s.authorize(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// A non-200 is not an empty stream, and treating it as one is worse than
	// it sounds: the read ends immediately, this returns nil, and Run loops
	// straight back round with no backoff — a hot retry against a server that
	// is refusing us, reported as silence.
	if resp.StatusCode != http.StatusOK {
		return errors.New(describeStatus(s.base, resp.StatusCode))
	}

	// An initial tick so the caller reads the current state immediately,
	// matching the server's own SSE behavior.
	s.notify()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			s.notify()
		}
	}
	return sc.Err()
}

func (s *Source) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return nil, err
	}
	s.authorize(req)
	return s.client.Do(req)
}
