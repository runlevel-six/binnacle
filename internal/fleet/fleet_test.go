package fleet

import (
	"errors"
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

func newTracked(ns, name string) *tracked {
	return &tracked{
		discovered: Discovered{Namespace: ns, Name: name},
		store:      store.New(),
		registry:   plugin.NewRegistry(),
		cancel:     func() {},
	}
}

func fleetOf(ts ...*tracked) *Fleet {
	f := &Fleet{clusters: map[string]*tracked{}, changed: make(chan struct{}, 1)}
	for _, t := range ts {
		f.clusters[t.discovered.Key()] = t
	}
	return f
}

// A cluster nothing has been published for is unknown, not healthy. A fleet
// page that paints an unreported cluster green is worse than one that will not
// load, because it answers the question wrongly instead of not answering it.
func TestView_UnreportedClusterIsLoading(t *testing.T) {
	v := fleetOf(newTracked("capi", "tenant-01")).View()
	if len(v) != 1 {
		t.Fatalf("got %d clusters", len(v))
	}
	if v[0].Status != health.StatusLoading {
		t.Errorf("status = %v, want loading", v[0].Status)
	}
	if len(v[0].Cells) != 0 {
		t.Errorf("got %d cells from an empty store", len(v[0].Cells))
	}
}

// A cluster that cannot be read reports the reason and nothing else. The row
// must not carry counts as well, or a reader sees numbers under a connection
// error and has no way to tell how old they are.
func TestView_UnreadableClusterCarriesOnlyTheProblem(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.discovered.Err = errors.New("read tenant-01-kubeconfig: secret not found")
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "node-1", Status: "Ready"}},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if v.Problem == "" {
		t.Fatal("expected a problem")
	}
	if v.Status != health.StatusErr {
		t.Errorf("status = %v, want err", v.Status)
	}
	if v.Nodes.Total != 0 || len(v.Cells) != 0 {
		t.Errorf("an unreadable cluster reported data: %+v", v)
	}
}

// An unreachable API server is a problem in the same sense a missing secret is,
// and must not be reported as a healthy cluster with no data.
func TestView_UnreachableManagementIsAProblem(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	no := false
	tr.reachable, tr.serverErr = &no, errors.New("dial tcp: i/o timeout")
	v := fleetOf(tr).View()[0]
	if v.Problem == "" || v.Status != health.StatusErr {
		t.Errorf("got %+v, want an errored row", v)
	}
}

// A cluster that has been reached but has not published yet is still loading —
// reachability alone is not health.
func TestView_ReachableButSilentStaysLoading(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	yes := true
	tr.reachable = &yes
	if v := fleetOf(tr).View()[0]; v.Status != health.StatusLoading || v.Problem != "" {
		t.Errorf("got %+v, want a loading row with no problem", v)
	}
}

// The worst cluster sorts first. A fleet page is read top-down under time
// pressure, and the one that needs attention should not need scrolling to.
func TestView_WorstFirst(t *testing.T) {
	healthy := newTracked("capi", "aaa-healthy")
	healthy.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "n1", Status: "Ready"}},
		UpdatedAt: time.Now(),
	})
	broken := newTracked("capi", "zzz-broken")
	broken.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "n1", Status: "NotReady"}},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(healthy, broken).View()
	if v[0].Name != "zzz-broken" {
		t.Errorf("first row is %q; the broken cluster should lead", v[0].Name)
	}
	if v[1].Status != health.StatusOK {
		t.Errorf("second row status = %v, want ok", v[1].Status)
	}
}

// Two clusters at the same severity sort by name, so the page does not
// reshuffle itself under a reader between updates.
func TestView_TiesSortByName(t *testing.T) {
	f := fleetOf(newTracked("capi", "b"), newTracked("capi", "a"), newTracked("other", "a"))
	v := f.View()
	got := []string{v[0].Namespace + "/" + v[0].Name, v[1].Namespace + "/" + v[1].Name, v[2].Namespace + "/" + v[2].Name}
	want := []string{"capi/a", "capi/b", "other/a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v want %v", got, want)
		}
	}
}

// The node count honours the profile's expected cordons for the same reason the
// health cell does: a fleet of hypervisors cordoned by design must not read as
// a fleet-wide drain.
func TestView_ExpectedCordonsAreNotCounted(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "cp-1", Status: "Ready", Role: "control-plane"},
			{Name: "compute-1", Status: "Ready", Role: "compute", Cordoned: true},
			{Name: "compute-2", Status: "Ready", Role: "compute", Cordoned: true},
		},
		UpdatedAt: time.Now(),
	})
	f := fleetOf(tr)
	f.opts.Profile = profile.Profile{
		NodeRoles: profile.NodeRoles{CordonExpected: []string{"compute"}},
	}

	v := f.View()[0]
	if v.Nodes.Total != 3 || v.Nodes.Ready != 3 {
		t.Errorf("nodes = %+v, want 3/3", v.Nodes)
	}
	if v.Nodes.Cordoned != 0 {
		t.Errorf("counted %d expected cordons; should be none", v.Nodes.Cordoned)
	}
	if v.Status != health.StatusOK {
		t.Errorf("status = %v, want ok", v.Status)
	}

	// Without the profile setting the same fleet reports the cordons, which is
	// right on a stock cluster where a cordon really is a drain.
	f.opts.Profile = profile.Profile{}
	if v := f.View()[0]; v.Nodes.Cordoned != 2 {
		t.Errorf("unconfigured: counted %d cordons, want 2", v.Nodes.Cordoned)
	}
}

// Cluster-level facts come from the management side, matched by name: a
// collector narrowed to one cluster should publish one, but the row must not
// pick up a neighbour's numbers if it ever publishes more.
func TestView_ClusterFactsMatchByName(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyMgmtClusters, model.Snapshot[model.Cluster]{
		Items: []model.Cluster{
			{Namespace: "capi", Name: "tenant-02", Version: "v1.32.0",
				ControlPlane: model.ReplicaBucket{Desired: 9, Ready: 9}},
			{Namespace: "capi", Name: "tenant-01", Version: "v1.31.4", Phase: "Provisioned",
				ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3},
				Workers:      model.ReplicaBucket{Desired: 5, Ready: 4}},
		},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if v.Version != "v1.31.4" || v.Phase != "Provisioned" {
		t.Errorf("got version %q phase %q", v.Version, v.Phase)
	}
	if v.ControlPlane.Desired != 3 || v.Workers.Ready != 4 {
		t.Errorf("picked up another cluster's replicas: %+v / %+v", v.ControlPlane, v.Workers)
	}
}

// Rollout state is sextant's, not re-derived here.
func TestView_RolloutComesFromSextant(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyMgmtKCPs, model.Snapshot[model.KubeadmControlPlane]{
		Items: []model.KubeadmControlPlane{
			{Namespace: "capi", Name: "tenant-01-cp", DesiredReplicas: 3, UpToDateReplicas: 1},
		},
		UpdatedAt: time.Now(),
	})
	v := fleetOf(tr).View()[0]
	if !v.Rolling() {
		t.Error("a control plane with replicas left to update should read as rolling")
	}
}

// notify never blocks, however slow the reader. A collector stalling on a
// browser that went away would take the cluster's data with it.
func TestNotify_DoesNotBlock(t *testing.T) {
	f := fleetOf()
	done := make(chan struct{})
	go func() {
		for range 100 {
			f.notify()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked with nobody reading")
	}
}

func TestDiscoveredKey(t *testing.T) {
	if got := (Discovered{Namespace: "capi", Name: "tenant-01"}).Key(); got != "capi/tenant-01" {
		t.Errorf("got %q", got)
	}
}
