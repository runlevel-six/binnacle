package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

	var entries []wire.Entry
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var dataLines []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		} else if line == "" && len(dataLines) > 0 {
			payload := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]
			if err := json.Unmarshal([]byte(payload), &entries); err != nil {
				continue
			}
			wire.Load(entries, st)
		}
	}
	return sc.Err()
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
