package fleet

import (
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// arkish is a profile shaped like a real site's: workloads pinned on the
// clusters it provisions, controllers pinned on the cluster doing the
// provisioning, and one name that exists on both.
func arkish() profile.Profile {
	return profile.Profile{
		Name: "arkish",
		CriticalWorkloads: []profile.CriticalWorkload{
			{Kind: "StatefulSet", Namespace: "openstack", Name: "ovn-ovsdb-nb"},
			// Present on the management cluster too, under the same namespace
			// and name, and a different deployment entirely.
			{Kind: "Deployment", Namespace: "traefik", Name: "traefik"},
		},
		ManagementWorkloads: []profile.CriticalWorkload{
			{Kind: "Deployment", Namespace: "capi-system", Name: "capi-controller-manager"},
		},
	}
}

// managementPods is what the management cluster runs: its own controllers and
// its own ingress. None of the workload cluster's components are here, because
// they are not supposed to be.
func managementPods() []model.Pod {
	return []model.Pod{
		{Namespace: "capi-system", Name: "capi-controller-manager-7d9f-abcde",
			ReadyReady: 1, ReadyTotal: 1, Status: "Running", IsHealthy: true},
		{Namespace: "traefik", Name: "traefik-6d8c-fghij",
			ReadyReady: 1, ReadyTotal: 1, Status: "Running", IsHealthy: true},
	}
}

// The defect this exists for: the management section read CriticalWorkloads,
// which describes the clusters this one provisions. Every entry then reported
// against the wrong cluster — absent for the components that live elsewhere,
// and, for a name that happens to exist on both, *healthy*, which is a green
// verdict about an object nobody asked about.
func TestControllerHealth_ReadsManagementWorkloadsOnly(t *testing.T) {
	ch := buildControllerHealth(managementPods(), arkish())

	if len(ch.Critical) != 1 {
		t.Fatalf("pinned %d workloads, want only the management one: %+v", len(ch.Critical), ch.Critical)
	}
	got := ch.Critical[0]
	if got.Name != "capi-controller-manager" {
		t.Errorf("pinned %q, want capi-controller-manager", got.Name)
	}
	if got.Absent || got.Ready != 1 {
		t.Errorf("controller should be healthy: %+v", got)
	}

	for _, pin := range ch.Critical {
		if pin.Namespace == "openstack" {
			t.Errorf("a workload cluster's component is pinned on the management cluster: %+v", pin)
		}
		if pin.Namespace == "traefik" {
			t.Errorf("traefik/traefik exists on both clusters; pinning the workload "+
				"cluster's entry here reports the management cluster's own as healthy: %+v", pin)
		}
	}
}

// A profile with no management workloads must produce no rows, so the section
// renders nothing at all. No table beats a table of wrong rows.
func TestControllerHealth_NoManagementWorkloadsMeansNoTable(t *testing.T) {
	prof := arkish()
	prof.ManagementWorkloads = nil

	ch := buildControllerHealth(managementPods(), prof)
	if len(ch.Critical) != 0 {
		t.Errorf("pinned %d workloads for a profile declaring none: %+v", len(ch.Critical), ch.Critical)
	}
}

// The other half of the same defect: critical workloads belong on the cluster
// pages, and the web UI showed them nowhere.
func TestClusterDetail_PinsTheProfilesCriticalWorkloads(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{UpdatedAt: time.Now(),
		Items: []model.Pod{
			{Namespace: "openstack", Name: "ovn-ovsdb-nb-0",
				ReadyReady: 1, ReadyTotal: 1, Status: "Running", IsHealthy: true},
			// traefik is pinned and has no pods here: the case an
			// unhealthy-only list cannot report at all.
			{Namespace: "example", Name: "api-0", Status: "CrashLoopBackOff", Restarts: 9},
		}})

	var d ClusterDetail
	d.readPods(s, arkish())

	if len(d.CriticalWorkloads) != 2 {
		t.Fatalf("pinned %d workloads, want the profile's two: %+v", len(d.CriticalWorkloads), d.CriticalWorkloads)
	}
	if got := d.CriticalWorkloads[0]; got.Absent || got.Ready != 1 {
		t.Errorf("ovn-ovsdb-nb should be healthy: %+v", got)
	}
	if got := d.CriticalWorkloads[1]; !got.Absent {
		t.Errorf("traefik has no pods on this cluster and must read absent: %+v", got)
	}
	if len(d.UnhealthyPods) != 1 {
		t.Errorf("the unhealthy list should be unaffected: %+v", d.UnhealthyPods)
	}
}

// A cordon the profile declares expected is the steady state, and the node
// table said otherwise while the count beside it said nothing was wrong — one
// page making two claims about the same cordon.
func TestNodeRows_ExpectedCordonIsNotNews(t *testing.T) {
	prof := profile.Profile{
		Name: "arkish",
		NodeRoles: profile.NodeRoles{
			LabelKeys:      []string{"site/role"},
			Display:        map[string]string{"compute": "Compute"},
			CordonExpected: []string{"compute"},
		},
	}
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{UpdatedAt: time.Now(),
		Items: []model.Node{
			{Name: "compute-1", Role: "compute", Status: "Ready", Cordoned: true},
			{Name: "compute-2", Role: "compute", Status: "NotReady", Cordoned: true},
			{Name: "ctl-1", Role: "controller", Status: "Ready", Cordoned: true},
		}})

	var d ClusterDetail
	d.readNodes(s, prof)

	byName := map[string]NodeRow{}
	for _, r := range append(append([]NodeRow{}, d.NodeRows.Shown...), d.NodeRows.Quiet...) {
		byName[r.Name] = r
	}

	if r := byName["compute-1"]; r.CordonNews {
		t.Error("a cordoned, Ready compute node is the steady state here and must not be news")
	}
	if r := byName["compute-2"]; !r.CordonNews {
		t.Error("cordoned and NotReady is news whatever the role")
	}
	if r := byName["ctl-1"]; !r.CordonNews {
		t.Error("a control-plane cordon is not covered by the exemption and is news")
	}
	if got := byName["compute-1"].RoleLabel; got != "Compute" {
		t.Errorf("role label %q, want the profile's display name Compute", got)
	}
	if got := byName["ctl-1"].RoleLabel; got != "controller" {
		t.Errorf("role label %q, want the raw role when the profile names no display for it", got)
	}
}
