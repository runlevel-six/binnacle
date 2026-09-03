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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/wire"
)

// Source is a [web.Source] backed by a binnacle server's JSON API.
type Source struct {
	base    string
	client  *http.Client
	token   string
	changed chan struct{}
}

// New builds a Source pointing at base, which is the binnacle server's root
// URL (e.g. "http://binnacle:8080").
//
// token, when non-empty, is sent as a Bearer header on every request. A
// server running with --allow-unauthenticated does not need one.
func New(base, token string) *Source {
	return &Source{
		base:    strings.TrimRight(base, "/"),
		client:  &http.Client{},
		token:   token,
		changed: make(chan struct{}, 1),
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
// reached.
func (s *Source) View() []fleet.ClusterView {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := s.get(ctx, "/api/v1/fleet")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var fr struct {
		Clusters []fleet.ClusterView `json:"clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return nil
	}
	return fr.Clusters
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

// Management returns an empty ManagementView. The management cluster is a
// web-only feature: its node and controller health is rendered server-side
// and is not exposed through the API. A terminal client does not need it —
// the fleet screen shows one line per workload cluster, and a management
// view is a separate screen shape that does not exist yet.
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
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

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
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return s.client.Do(req)
}
