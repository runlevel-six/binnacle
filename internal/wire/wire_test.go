package wire

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
)

func TestDumpAndLoad_RoundTrip(t *testing.T) {
	src := store.New()
	now := time.Now()

	src.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{
			{Name: "machine-1", ClusterName: "test", Phase: "Running"},
			{Name: "machine-2", ClusterName: "test", Phase: "Provisioning"},
		},
		UpdatedAt: now,
	})
	src.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "node-1", Status: "Ready", Role: "control-plane"},
		},
		UpdatedAt: now,
	})
	src.Put(ceph.KeyState, ceph.State{
		Tier:      2,
		Status:    ceph.Status{Health: "HEALTH_OK", OSDs: ceph.OSDs{Total: 3, Up: 3, In: 3}},
		UpdatedAt: now,
	})

	entries := Dump(src)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	keys := make(map[string]bool)
	for _, e := range entries {
		keys[e.Key] = true
		if len(e.Data) == 0 {
			t.Errorf("entry %q has empty data", e.Key)
		}
	}
	for _, want := range []string{model.KeyMgmtMachines, model.KeyWorkloadNodes, ceph.KeyState} {
		if !keys[want] {
			t.Errorf("missing key %q", want)
		}
	}

	dst := store.New()
	Load(entries, dst)

	got, ok := store.Get[model.Snapshot[model.Machine]](dst, model.KeyMgmtMachines)
	if !ok {
		t.Fatal("machines not found after Load")
	}
	if len(got.Items) != 2 {
		t.Errorf("expected 2 machines, got %d", len(got.Items))
	}
	if got.Items[0].Name != "machine-1" {
		t.Errorf("first machine name: got %q want machine-1", got.Items[0].Name)
	}

	nodes, ok := store.Get[model.Snapshot[model.Node]](dst, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("nodes not found after Load")
	}
	if len(nodes.Items) != 1 {
		t.Errorf("expected 1 node, got %d", len(nodes.Items))
	}

	cephState, ok := store.Get[ceph.State](dst, ceph.KeyState)
	if !ok {
		t.Fatal("ceph state not found after Load")
	}
	if cephState.Status.Health != "HEALTH_OK" {
		t.Errorf("ceph health: got %q want HEALTH_OK", cephState.Status.Health)
	}
}

func TestDump_SortedByKey(t *testing.T) {
	s := store.New()
	s.Put("zebra", model.Snapshot[model.Node]{})
	s.Put("alpha", model.Snapshot[model.Node]{})
	s.Put("mid", model.Snapshot[model.Node]{})

	entries := Dump(s)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Key != "alpha" || entries[1].Key != "mid" || entries[2].Key != "zebra" {
		t.Errorf("entries not sorted: %s, %s, %s", entries[0].Key, entries[1].Key, entries[2].Key)
	}
}

func TestLoad_UnknownKeySkipped(t *testing.T) {
	s := store.New()
	entries := []Entry{
		{Key: "unknown/future", Data: json.RawMessage(`{}`)},
		{Key: model.KeyWorkloadNodes, Data: json.RawMessage(`{"Items":[{"Name":"n1"}],"UpdatedAt":"2026-01-01T00:00:00Z"}`)},
	}
	Load(entries, s)

	if _, ok := s.Raw("unknown/future"); ok {
		t.Error("unknown key should not be stored")
	}
	nodes, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("known key should be stored")
	}
	if len(nodes.Items) != 1 {
		t.Errorf("expected 1 node, got %d", len(nodes.Items))
	}
}

func TestDump_EmptyStore(t *testing.T) {
	entries := Dump(store.New())
	if entries != nil {
		t.Errorf("expected nil for empty store, got %d entries", len(entries))
	}
}

// TestLoad_InvalidJSONPublishesTheReason: unparseable data under a key the
// client knows is not the same as an unknown key. Skipping it would leave
// whatever the store already held, so the pane would keep rendering the last
// good snapshot as though it were current. The reason is stored instead.
//
// This test previously asserted the opposite — that nothing was stored — which
// is the behavior that let a stale snapshot outlive the cluster it described.
func TestLoad_InvalidJSONPublishesTheReason(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{{Name: "n1", Status: "Ready"}},
	})

	Load([]Entry{
		{Key: model.KeyWorkloadNodes, Data: json.RawMessage(`not valid json`)},
	}, s)

	got, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("the key was dropped, leaving the pane with no signal at all")
	}
	if got.Err == nil {
		t.Fatal("unparseable data decoded as a healthy snapshot")
	}
	if len(got.Items) != 0 {
		t.Errorf("the stale node survived unparseable data: %v", got.Items)
	}
}

func TestRoundTrip_AllCoreKeys(t *testing.T) {
	src := store.New()
	now := time.Now()

	src.Put(model.KeyMgmtClusters, model.Snapshot[model.Cluster]{
		Items:     []model.Cluster{{Name: "c1", Namespace: "ns", Phase: "Provisioned"}},
		UpdatedAt: now,
	})
	src.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{
		Items:     []model.Pod{{Name: "pod-1", Namespace: "default", IsHealthy: true}},
		UpdatedAt: now,
	})
	src.Put(cilium.KeyState, cilium.State{
		Tier:          2,
		AgentsReady:   5,
		AgentsDesired: 5,
		UpdatedAt:     now,
	})

	entries := Dump(src)
	dst := store.New()
	Load(entries, dst)

	clusters, ok := store.Get[model.Snapshot[model.Cluster]](dst, model.KeyMgmtClusters)
	if !ok || len(clusters.Items) != 1 {
		t.Errorf("clusters round-trip failed")
	}
	pods, ok := store.Get[model.Snapshot[model.Pod]](dst, model.KeyWorkloadPods)
	if !ok || len(pods.Items) != 1 {
		t.Errorf("pods round-trip failed")
	}
	cil, ok := store.Get[cilium.State](dst, cilium.KeyState)
	if !ok || cil.AgentsReady != 5 {
		t.Errorf("cilium round-trip failed: %+v", cil)
	}
}
