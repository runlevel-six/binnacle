package fleet

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/store"
)

func newTracked(ns, name string) *tracked {
	return &tracked{
		discovered: Discovered{Namespace: ns, Name: name},
		store:      store.New(),
		registry:   plugin.NewRegistry(),
		cancel:     func() {},
		startedAt:  time.Now(),
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

// The node count honors the profile's expected cordons for the same reason the
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

// The management side alone produces a completely plausible healthy card:
// Provisioned, Machines Running, hosts fine. Every signal that could contradict
// it comes from the workload cluster, and a health cell with no data is
// omitted rather than shown as missing — so a cluster whose workload side is
// unreadable would otherwise render green with three badges instead of seven.
func TestView_UnreadableWorkloadSideCannotRenderHealthy(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyMgmtClusters, model.Snapshot[model.Cluster]{
		Items: []model.Cluster{{Namespace: "capi", Name: "tenant-01", Phase: "Provisioned",
			ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3}}},
		UpdatedAt: time.Now(),
	})
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Err: errors.New("Get \"https://10.0.0.1:6443/api/v1/nodes\": dial tcp: i/o timeout"),
	})

	v := fleetOf(tr).View()[0]
	if v.Status == health.StatusOK {
		t.Error("a cluster whose workload side cannot be read rendered as healthy")
	}
	if v.WorkloadProblem == "" {
		t.Error("no reason given for the missing workload data")
	}
	if v.NodesKnown {
		t.Error("node counts claimed to be known")
	}
}

// A workload cluster that reports an empty node list is not an empty cluster;
// it is a credential that resolved and a connection that is not working.
func TestView_EmptyNodeListIsAFault(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{UpdatedAt: time.Now()})
	if v := fleetOf(tr).View()[0]; v.Status != health.StatusWarn || v.WorkloadProblem == "" {
		t.Errorf("got status %v problem %q; want a warned row explaining itself", v.Status, v.WorkloadProblem)
	}
}

// A source that says it is not ready yet, and why, is reporting a state rather
// than a fault. Escalating that would paint the whole fleet amber on restart.
func TestView_WarmingWorkloadIsNotAFault(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Note: "waiting for the node cache to sync", UpdatedAt: time.Now(),
	})
	if v := fleetOf(tr).View()[0]; v.Status == health.StatusWarn {
		t.Error("a warming source was reported as a fault")
	}
}

// A cluster built without a ClusterClass has no .spec.topology and so no
// version on its Cluster object. The control plane's version is the cluster's
// version by any useful definition, and "version unknown" on every card is a
// worse answer than the one we can derive.
func TestView_VersionFallsBackToTheControlPlane(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyMgmtClusters, model.Snapshot[model.Cluster]{
		Items:     []model.Cluster{{Namespace: "capi", Name: "tenant-01", Phase: "Provisioned"}},
		UpdatedAt: time.Now(),
	})
	tr.store.Put(model.KeyMgmtKCPs, model.Snapshot[model.KubeadmControlPlane]{
		Items: []model.KubeadmControlPlane{{
			Namespace: "capi", Name: "tenant-01-cp", Version: "v1.36.2",
			DesiredReplicas: 3, ReadyReplicas: 3, UpToDateReplicas: 3,
		}},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if v.Version != "v1.36.2" {
		t.Errorf("version = %q, want the control plane's", v.Version)
	}
	if v.ControlPlane.Desired != 3 || v.ControlPlane.Ready != 3 {
		t.Errorf("control plane replicas not derived: %+v", v.ControlPlane)
	}
	if len(v.Pools) != 1 || v.Pools[0].Role != "Control Plane" {
		t.Errorf("pools = %+v", v.Pools)
	}
}

// Pools carry their own versions, which during an upgrade is the progress
// report the cluster-level version cannot give.
func TestView_PoolsCarryTheirOwnVersions(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyMgmtKCPs, model.Snapshot[model.KubeadmControlPlane]{
		Items:     []model.KubeadmControlPlane{{Name: "cp", Version: "v1.36.2", DesiredReplicas: 3, ReadyReplicas: 3}},
		UpdatedAt: time.Now(),
	})
	tr.store.Put(model.KeyMgmtMachineDeployments, model.Snapshot[model.MachineDeployment]{
		Items: []model.MachineDeployment{
			{Name: "workers-b", Version: "v1.35.4", DesiredReplicas: 2, ReadyReplicas: 2},
			{Name: "workers-a", Version: "v1.36.2", DesiredReplicas: 4, ReadyReplicas: 4},
		},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if len(v.Pools) != 3 {
		t.Fatalf("got %d pools, want 3", len(v.Pools))
	}
	// Control plane first: it is the half that has to be healthy for the other
	// half to matter. Workers then sort by name so the card is stable.
	if v.Pools[0].Role != "Control Plane" || v.Pools[1].Name != "workers-a" || v.Pools[2].Name != "workers-b" {
		t.Errorf("pool order wrong: %+v", v.Pools)
	}
	if v.Workers.Desired != 6 || v.Workers.Ready != 6 {
		t.Errorf("worker totals = %+v, want 6/6", v.Workers)
	}
}

// Capacity is committed requests against allocatable, summed over the nodes.
func TestView_CapacityAndWorkloads(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "n1", Status: "Ready", AllocatableCPU: 4000, RequestedCPU: 1000,
				AllocatableMemory: 8 << 30, RequestedMemory: 2 << 30},
			{Name: "n2", Status: "Ready", AllocatableCPU: 4000, RequestedCPU: 3000,
				AllocatableMemory: 8 << 30, RequestedMemory: 6 << 30},
		},
		UpdatedAt: time.Now(),
	})
	tr.store.Put(model.KeyWorkloadWorkloads, model.Snapshot[model.Workload]{
		Items: []model.Workload{
			{Kind: "Deployment", Ready: 2, Desired: 2},
			{Kind: "Deployment", Ready: 1, Desired: 3},
			{Kind: "DaemonSet", Ready: 5, Desired: 5},
		},
		UpdatedAt: time.Now(),
	})
	tr.store.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{
		Items: []model.Pod{
			{Name: "a", IsHealthy: true}, {Name: "b"}, {Name: "c"},
		},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if !v.NodesKnown || v.Nodes.Ready != 2 {
		t.Errorf("nodes = %+v known=%v", v.Nodes, v.NodesKnown)
	}
	if got := v.Capacity.CPUPercent(); got != 50 {
		t.Errorf("cpu = %d%%, want 50", got)
	}
	if got := v.Capacity.MemPercent(); got != 50 {
		t.Errorf("mem = %d%%, want 50", got)
	}
	if len(v.Workloads) != 2 || v.Workloads[0].Kind != "DaemonSet" {
		t.Fatalf("workloads = %+v", v.Workloads)
	}
	if d := v.Workloads[1]; d.Kind != "Deployment" || d.Ready != 1 || d.Total != 2 {
		t.Errorf("deployments = %+v, want 1/2 ready", d)
	}
	if v.UnhealthyPods != 2 {
		t.Errorf("unhealthy pods = %d, want 2", v.UnhealthyPods)
	}
}

// Silence needs a clock against it. A workload cluster that will never report —
// because nothing can reach it — otherwise sits in the startup branch for the
// life of the process, and renders a permanently green card carrying a note
// nobody reads as a failure.
func TestView_SilenceBecomesAFaultAfterTheGracePeriod(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.startedAt = time.Now().Add(-2 * workloadGrace)

	v := fleetOf(tr).View()[0]
	if v.Status != health.StatusWarn {
		t.Errorf("status = %v; a cluster silent past the grace period is not healthy", v.Status)
	}
	if !strings.Contains(v.WorkloadProblem, "nothing has been read") {
		t.Errorf("problem = %q", v.WorkloadProblem)
	}
}

// Inside the grace period the same silence is just a cluster starting up.
func TestView_SilenceInsideTheGracePeriodIsNotAFault(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	if v := fleetOf(tr).View()[0]; v.Status == health.StatusWarn {
		t.Error("a cluster that has only just started was reported as a fault")
	}
}

// A probe that came back with a reason beats waiting out the grace period: the
// reason is what an operator can act on, and it is available immediately.
func TestView_WorkloadProbeErrorIsReportedAtOnce(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.workloadErr = errors.New(`Get "https://192.0.2.10:6443/version": dial tcp: i/o timeout`)
	tr.workloadProbe = true

	v := fleetOf(tr).View()[0]
	if v.Status != health.StatusWarn {
		t.Errorf("status = %v, want warn", v.Status)
	}
	if !strings.Contains(v.WorkloadProblem, "i/o timeout") {
		t.Errorf("problem = %q; want the probe's own reason", v.WorkloadProblem)
	}
}

// Once the workload cluster is actually reporting, a stale probe error must not
// keep contradicting the data on the card.
func TestView_ProbeErrorIsIgnoredOnceNodesArrive(t *testing.T) {
	tr := newTracked("capi", "tenant-01")
	tr.workloadErr, tr.workloadProbe = errors.New("transient failure at startup"), true
	tr.store.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "n1", Status: "Ready"}},
		UpdatedAt: time.Now(),
	})

	v := fleetOf(tr).View()[0]
	if v.WorkloadProblem != "" {
		t.Errorf("problem = %q; nodes arrived, so the probe error is stale", v.WorkloadProblem)
	}
	if !v.NodesKnown || v.Status != health.StatusOK {
		t.Errorf("got status %v known=%v", v.Status, v.NodesKnown)
	}
}
