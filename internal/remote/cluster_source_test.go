package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
)

func TestClusterSource_Name(t *testing.T) {
	s := NewClusterSource("http://localhost", "", "ns", "name")
	if s.Name() != "remote" {
		t.Errorf("Name: got %q want remote", s.Name())
	}
}

func TestClusterSource_Detect_ClusterExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/clusters/ns/name/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, "", "ns", "name")
	ok, err := s.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected detected=true")
	}
}

func TestClusterSource_Detect_ClusterNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, "", "ns", "name")
	ok, err := s.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected detected=false for 404")
	}
}

func TestClusterSource_Detect_ServerUnreachable(t *testing.T) {
	s := NewClusterSource("http://127.0.0.1:0", "", "ns", "name")
	_, err := s.Detect(context.Background())
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestClusterSource_Run_StoresEntries(t *testing.T) {
	now := time.Now()
	entries := []wire.Entry{
		{
			Key: model.KeyWorkloadNodes,
			Data: mustJSON(model.Snapshot[model.Node]{
				Items:     []model.Node{{Name: "node-1", Status: "Ready"}},
				UpdatedAt: now,
			}),
		},
		{
			Key: ceph.KeyState,
			Data: mustJSON(ceph.State{
				Status:    ceph.Status{Health: "HEALTH_WARN"},
				UpdatedAt: now,
			}),
		},
	}

	payload, _ := json.Marshal(entries)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload)
		w.(http.Flusher).Flush()
		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, "", "ns", "name")
	st := store.New()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Run(ctx, st)

	nodes, ok := store.Get[model.Snapshot[model.Node]](st, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("nodes not found in store after Run")
	}
	if len(nodes.Items) != 1 || nodes.Items[0].Name != "node-1" {
		t.Errorf("unexpected nodes: %+v", nodes.Items)
	}

	cephState, ok := store.Get[ceph.State](st, ceph.KeyState)
	if !ok {
		t.Fatal("ceph state not found in store after Run")
	}
	if cephState.Status.Health != "HEALTH_WARN" {
		t.Errorf("ceph health: got %q want HEALTH_WARN", cephState.Status.Health)
	}
}

func TestClusterSource_Run_ReconnectsOnError(t *testing.T) {
	now := time.Now()
	entries := []wire.Entry{
		{
			Key: model.KeyWorkloadNodes,
			Data: mustJSON(model.Snapshot[model.Node]{
				Items:     []model.Node{{Name: "node-1"}},
				UpdatedAt: now,
			}),
		},
	}
	payload, _ := json.Marshal(entries)

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, "", "ns", "name")
	st := store.New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Run(ctx, st)

	if attempts < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts)
	}
	nodes, ok := store.Get[model.Snapshot[model.Node]](st, model.KeyWorkloadNodes)
	if !ok || len(nodes.Items) != 1 {
		t.Errorf("store should have nodes after reconnect: %+v", nodes)
	}
}

func TestClusterSource_Run_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := NewClusterSource(srv.URL, "", "ns", "name")
	st := store.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := s.Run(ctx, st)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
