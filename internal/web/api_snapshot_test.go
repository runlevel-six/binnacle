package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
)

// snapshotFleet is a fakeFleet that returns real store snapshots.
type snapshotFleet struct {
	fakeFleet
	entries []wire.Entry
}

func (f *snapshotFleet) StoreSnapshot(_, _ string) ([]wire.Entry, bool) {
	return f.entries, true
}

func serveWithSnapshot(t *testing.T, entries []wire.Entry) http.Handler {
	t.Helper()
	f := &snapshotFleet{
		fakeFleet: fakeFleet{
			clusters: []fleet.ClusterView{
				{Namespace: "capi", Name: "tenant-01", Status: health.StatusOK},
			},
			changed: make(chan struct{}, 1),
		},
		entries: entries,
	}
	s, err := New(f, auth.Open{}, "test", "site-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func TestAPIClusterSnapshot_ReturnsEntries(t *testing.T) {
	s := store.New()
	now := time.Now()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "node-1", Status: "Ready"}},
		UpdatedAt: now,
	})
	s.Put(ceph.KeyState, ceph.State{
		Status:    ceph.Status{Health: "HEALTH_OK"},
		UpdatedAt: now,
	})
	entries := wire.Dump(s)

	h := serveWithSnapshot(t, entries)
	rec := get(t, h, "/api/v1/clusters/capi/tenant-01/snapshot")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusOK)
	}
	var got []wire.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
}

func TestAPIClusterSnapshot_404ForUnknownCluster(t *testing.T) {
	h := serve(t)
	rec := get(t, h, "/api/v1/clusters/unknown/cluster/snapshot")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAPIClusterStream_FirstFrameImmediate(t *testing.T) {
	s := store.New()
	now := time.Now()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "node-1"}},
		UpdatedAt: now,
	})
	entries := wire.Dump(s)

	h := serveWithSnapshot(t, entries)

	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/clusters/capi/tenant-01/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type: got %q want text/event-stream", ct)
	}
}

// TestAPIClusterStream_404ForUnknownCluster verifies that a non-existent
// cluster produces a 404 rather than an empty stream.
func TestAPIClusterStream_404ForUnknownCluster(t *testing.T) {
	// fakeFleet returns (nil, false) from StoreSnapshot, which the handler
	// should translate to 404.
	h := serve(t)
	rec := get(t, h, "/api/v1/clusters/unknown/cluster/stream")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}
