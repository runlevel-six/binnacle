package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// ClusterSource is a [plugin.Source] that streams one cluster's raw store
// contents from a binnacle server's SSE endpoint.
//
// It is the per-cluster data source for sextant's --server mode: when an
// operator drills into a cluster from the fleet list, the router builds a
// fresh store and registry, registers ClusterSource as the sole source,
// and starts Run. The SSE stream delivers wire entries that Load decodes
// into typed snapshots, populating the store the same panes read in local
// mode.
//
// Detect checks that the cluster exists on the server. Run opens the SSE
// stream at /api/v1/clusters/{ns}/{name}/stream and blocks until the
// context is canceled.
type ClusterSource struct {
	base   string
	token  TokenFunc
	ns     string
	name   string
	client *http.Client

	// mu guards problem, which a caller may read from another goroutine.
	mu sync.Mutex
	// problem is why the stream last failed, or empty when it is healthy.
	//
	// Without it a refused stream and a quiet one are the same picture: the
	// store stays empty, every pane says "loading", and nothing anywhere says
	// which. Run retries either way; this is how the reason escapes.
	problem string
}

// Problem reports why the cluster stream last failed, or empty when it is
// working.
func (s *ClusterSource) Problem() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.problem
}

func (s *ClusterSource) setProblem(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problem = p
}

// NewClusterSource builds a source for one cluster on the given server.
func NewClusterSource(base string, token TokenFunc, namespace, name string) *ClusterSource {
	return &ClusterSource{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		ns:     namespace,
		name:   name,
		client: &http.Client{},
	}
}

// Name returns the plugin name used for registration.
func (s *ClusterSource) Name() string { return "remote" }

// Detect verifies that the cluster exists on the server by requesting
// a single snapshot. A 404 means the cluster is gone; any other error
// means the server is unreachable. Both return (false, err).
func (s *ClusterSource) Detect(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/clusters/%s/%s/snapshot", s.base, s.ns, s.name), nil)
	if err != nil {
		return false, err
	}
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return true, nil
}

// Run opens the SSE stream and decodes snapshots into the store until
// the context is canceled. On connection failure it reconnects after a
// brief delay, matching the fleet source's behavior.
func (s *ClusterSource) Run(ctx context.Context, st *store.Store) error {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.stream(ctx, st); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.setProblem(fmt.Sprintf("cannot stream %s/%s from %s: %v", s.ns, s.name, s.base, err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
	}
}

func (s *ClusterSource) stream(ctx context.Context, st *store.Store) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/clusters/%s/%s/stream", s.base, s.ns, s.name), nil)
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

	// Same reason as the fleet stream: a refused request reads as an empty
	// stream, which returns nil, which Run retries with no delay at all.
	if resp.StatusCode != http.StatusOK {
		return errors.New(describeStatus(s.base, resp.StatusCode))
	}

	var entries []wire.Entry
	// A bufio.Reader rather than a Scanner: the server sends one cluster's
	// entire store as a single SSE data line, and a Scanner refuses any line
	// over its buffer. That ceiling was 4 MiB, and a real OpenStack undercloud
	// measured 4,178,484 bytes — 0.4% under it. Crossing it failed the read,
	// which retried forever, silently, so every pane showed "loading" for the
	// life of the process. maxFrame is the same protection with room to be
	// wrong in.
	br := bufio.NewReaderSize(resp.Body, 64*1024)

	var dataLines []string
	for {
		line, err := readLine(br, maxFrame)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch {
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		case line == "" && len(dataLines) > 0:
			payload := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]
			if err := json.Unmarshal([]byte(payload), &entries); err != nil {
				continue
			}
			wire.Load(entries, st)
			s.setProblem("")
		}
	}
}

// maxFrame bounds one SSE line, so a server that never sends a newline cannot
// exhaust this process's memory.
//
// 64 MiB against a measured 4 MiB: an order of magnitude of headroom, because
// the store grows with the cluster and with how many namespaces the site
// profile watches for events, and the failure at the boundary is invisible.
const maxFrame = 64 << 20

// readLine reads one newline-terminated line of any length up to limit,
// returning it without the trailing newline.
//
// bufio.Reader.ReadString would do this with no limit at all; the limit is the
// point. A line that exceeds it returns an error naming the size, because the
// one thing this must never do is fail quietly.
func readLine(br *bufio.Reader, limit int) (string, error) {
	var b []byte
	for {
		chunk, more, err := br.ReadLine()
		if err != nil {
			return "", err
		}
		if len(b)+len(chunk) > limit {
			return "", fmt.Errorf("server sent a line over %d bytes, which this client will not buffer", limit)
		}
		b = append(b, chunk...)
		if !more {
			return string(b), nil
		}
	}
}

var _ plugin.Source = (*ClusterSource)(nil)

// authorize stamps the current credential on a request, if there is one.
func (s *ClusterSource) authorize(req *http.Request) {
	if s.token == nil {
		return
	}
	if tok := s.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
