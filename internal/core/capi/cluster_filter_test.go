package capi

import (
	"errors"
	"testing"

	"github.com/runlevel-six/sextant/internal/core/model"
)

func machinesSnap(clusters ...string) model.Snapshot[model.Machine] {
	snap := model.Snapshot[model.Machine]{}
	for i, c := range clusters {
		snap.Items = append(snap.Items, model.Machine{
			Namespace: "machines", Name: string(rune('a' + i)), ClusterName: c,
		})
	}
	return snap
}

func clusterNames(snap model.Snapshot[model.Machine]) []string {
	out := make([]string, 0, len(snap.Items))
	for _, m := range snap.Items {
		out = append(out, m.ClusterName)
	}
	return out
}

func machineCluster(m model.Machine) string { return m.ClusterName }

// The case this exists for: one management cluster owning two clusters, where the
// workload panes read one of them.
func TestOfCluster_NarrowsToOneCluster(t *testing.T) {
	snap := machinesSnap("tenant-01-cluster", "tenant-02-cluster", "tenant-01-cluster")

	got := ofCluster(snap, "tenant-01", machineCluster)
	if len(got.Items) != 2 {
		t.Fatalf("got %v, want the two tenant-01 machines", clusterNames(got))
	}
	for _, m := range got.Items {
		if m.ClusterName != "tenant-01-cluster" {
			t.Errorf("kept a machine from %q", m.ClusterName)
		}
	}
}

// A Cluster object carries a suffix the context name does not, so requiring
// equality would match nothing and empty every Cluster API pane — the one failure
// this filter must never have.
func TestOfCluster_MatchesTheConventionalSuffix(t *testing.T) {
	snap := machinesSnap("tenant-01-cluster")
	if got := ofCluster(snap, "tenant-01", machineCluster); len(got.Items) != 1 {
		t.Errorf("a derived name must match the suffixed Cluster name, got %v", clusterNames(got))
	}
	// The exact name still works, for a profile that pins it.
	if got := ofCluster(snap, "tenant-01-cluster", machineCluster); len(got.Items) != 1 {
		t.Errorf("an exact name should match too, got %v", clusterNames(got))
	}
}

// Hiding an object because its provenance is unclear is the worse mistake: an
// extra row is visible, a missing one looks like the cluster has no such Machine.
func TestOfCluster_KeepsUnlabelledItems(t *testing.T) {
	snap := machinesSnap("tenant-01-cluster", "", "tenant-02-cluster")

	got := ofCluster(snap, "tenant-01", machineCluster)
	if len(got.Items) != 2 {
		t.Fatalf("got %v, want the tenant-01 machine and the unlabelled one", clusterNames(got))
	}
}

func TestOfCluster_NoNameMeansNoFilter(t *testing.T) {
	snap := machinesSnap("a-cluster", "b-cluster")
	if got := ofCluster(snap, "", machineCluster); len(got.Items) != 2 {
		t.Errorf("an empty filter must pass everything, got %v", clusterNames(got))
	}
}

// An error snapshot carries no items and must keep its error rather than being
// rewritten into an empty success.
func TestOfCluster_PreservesErrors(t *testing.T) {
	snap := model.Snapshot[model.Machine]{Err: errors.New("forbidden: cannot list machines")}
	got := ofCluster(snap, "tenant-01", machineCluster)
	if got.Err == nil {
		t.Error("the error was dropped")
	}
}

// Clusters are filtered on their own name, since there is no label pointing at
// themselves.
func TestOfCluster_ClusterObjectsMatchOnName(t *testing.T) {
	snap := model.Snapshot[model.Cluster]{Items: []model.Cluster{
		{Namespace: "machines", Name: "tenant-01-cluster"},
		{Namespace: "machines", Name: "tenant-02-cluster"},
	}}

	got := ofCluster(snap, "tenant-01", func(c model.Cluster) string { return c.Name })
	if len(got.Items) != 1 || got.Items[0].Name != "tenant-01-cluster" {
		t.Errorf("got %+v", got.Items)
	}
}

// The host join has to stay global, which is why Metal3Machines and
// BareMetalHosts are not filtered: a host is unclaimed only when *no* cluster
// claims it, and hiding another cluster's Metal3Machines would report its hosts as
// free inventory ready to be provisioned over.
func TestUnclaimedHostsStayGlobalAcrossClusters(t *testing.T) {
	machines := []model.Machine{
		{Namespace: "machines", Name: "tenant-01-cp-1", ClusterName: "tenant-01-cluster",
			InfraKind: "Metal3Machine", InfraName: "tenant-01-cp-1-m3m"},
		{Namespace: "machines", Name: "tenant-02-cp-1", ClusterName: "tenant-02-cluster",
			InfraKind: "Metal3Machine", InfraName: "tenant-02-cp-1-m3m"},
	}
	m3ms := []model.Metal3Machine{
		{Namespace: "machines", Name: "tenant-01-cp-1-m3m", BMHNamespace: "machines", BMHName: "host-1"},
		{Namespace: "machines", Name: "tenant-02-cp-1-m3m", BMHNamespace: "machines", BMHName: "host-2"},
	}
	hosts := []model.BareMetalHost{
		{Namespace: "machines", Name: "host-1"},
		{Namespace: "machines", Name: "host-2"},
		{Namespace: "machines", Name: "host-3"},
	}

	// The Machines are narrowed to one cluster, as the watcher does...
	narrowed := ofCluster(model.Snapshot[model.Machine]{Items: machines}, "tenant-01", machineCluster)
	if len(narrowed.Items) != 1 {
		t.Fatalf("expected one machine, got %d", len(narrowed.Items))
	}

	// ...but claims are computed against every Metal3Machine, so the other
	// cluster's host is not offered up as free.
	unclaimed := UnclaimedHosts(m3ms, hosts)
	if len(unclaimed) != 1 || unclaimed[0].Name != "host-3" {
		names := make([]string, 0, len(unclaimed))
		for _, h := range unclaimed {
			names = append(names, h.Name)
		}
		t.Errorf("unclaimed = %v, want only host-3", names)
	}
}
