package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// bigSnapshot builds a store dump whose JSON exceeds n bytes, as one SSE line —
// which is how the server sends a cluster's whole store.
func bigSnapshot(t *testing.T, n int) string {
	t.Helper()
	pods := make([]model.Pod, 0, 32768)
	for len(pods) < 32768 {
		pods = append(pods, model.Pod{
			Namespace: "openstack",
			Name:      fmt.Sprintf("a-pod-with-a-realistic-length-of-name-%d-5f9c8d7b6a", len(pods)),
			Status:    "Running", IsHealthy: true, Node: "compute-node-11.site-a.example",
		})
	}
	data, err := json.Marshal(model.Snapshot[model.Pod]{Items: pods, UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal([]wire.Entry{{Key: model.KeyWorkloadPods, Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < n {
		t.Fatalf("fixture is %d bytes, want more than %d", len(payload), n)
	}
	return string(payload)
}

// The regression this exists for, measured on a real undercloud: the server
// sends one cluster's entire store as a single SSE line, and the client read it
// with a scanner capped at 4 MiB. dev1a's dump was 4,178,484 bytes — 15,820
// bytes under the ceiling. Crossing it failed the read, which retried forever
// in silence, so every pane said "loading" for the life of the process.
func TestClusterSource_AcceptsAFrameOverTheOldFourMebibyteCeiling(t *testing.T) {
	payload := bigSnapshot(t, 4<<20)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload)
		w.(http.Flusher).Flush()
		<-time.After(300 * time.Millisecond)
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, nil, "ns", "name")
	st := store.New()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.Run(ctx, st)

	snap, ok := store.Get[model.Snapshot[model.Pod]](st, model.KeyWorkloadPods)
	if !ok {
		t.Fatalf("a %d-byte frame did not reach the store; problem: %q", len(payload), s.Problem())
	}
	if len(snap.Items) == 0 {
		t.Error("the snapshot arrived empty")
	}
	if p := s.Problem(); p != "" {
		t.Errorf("a successful stream left a problem set: %q", p)
	}
}

// A frame past even the generous limit must say so rather than retrying
// quietly. The silence is what made the original bug cost an hour: a refused
// stream, a broken stream and a warming one all rendered as "loading".
func TestClusterSource_ReportsAFrameItWillNotBuffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: ")
		// More than maxFrame, written without a newline.
		chunk := strings.Repeat("x", 1<<20)
		for written := 0; written <= maxFrame; written += len(chunk) {
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, nil, "ns", "name")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.Run(ctx, store.New())

	if p := s.Problem(); !strings.Contains(p, "will not buffer") {
		t.Errorf("problem %q does not name the reason the stream failed", p)
	}
}

// An unreachable or refusing server must leave a reason behind too. Run retries
// forever either way; what changed is that the reason escapes the loop.
func TestSource_RecordsWhyTheSubscriptionFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := New(srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	if p := s.Problem(); !strings.Contains(p, "cannot subscribe") {
		t.Errorf("problem %q does not report the failed subscription", p)
	}
}
