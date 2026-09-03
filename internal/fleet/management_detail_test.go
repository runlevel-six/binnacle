package fleet

import (
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// mgmtEvent is one management-namespace event naming an object.
func mgmtEvent(objectName, reason string) model.Event {
	return model.Event{
		Namespace: "machines", Type: "Warning", Reason: reason,
		ObjectKind: "Machine", ObjectName: objectName,
		Message: reason + " on " + objectName, LastTimestamp: time.Now(),
	}
}

// The load-bearing correctness of this page, and the same trap the hosts and
// events panes each fell into once: the Cluster API namespace holds every
// cluster in the datacenter, so "the events nobody claims" has to be the
// complement of *every* cluster's names — not of the one cluster whose
// snapshot happened to be borrowed.
//
// Get it wrong and the management page reports one cluster's Machine failures
// as unclaimed management events, which is the same class of wrong as k8s00's
// page showing k8s01's control plane being paused.
func TestManagementEvents_ExcludesEveryClustersOwnEvents(t *testing.T) {
	shared := model.Snapshot[model.Event]{
		Items: []model.Event{
			mgmtEvent("tenant-01-cp-x7k2n", "DrainFailed"),
			mgmtEvent("tenant-02-compute-9fjd4", "VMMigrationFailed"),
			mgmtEvent("draino-watcher-7c9fd8b64c", "PolicyViolation"),
		},
		UpdatedAt: time.Now(),
	}

	a := newTracked("machines", "tenant-01")
	b := newTracked("machines", "tenant-02")
	for _, tr := range []*tracked{a, b} {
		// Both collectors watch the same namespace, so both stores hold the
		// same snapshot — which is what makes borrowing one of them sound.
		tr.store.Put(model.KeyMgmtEvents, shared)
	}
	a.store.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{{Name: "tenant-01-cp-x7k2n"}}, UpdatedAt: time.Now(),
	})
	b.store.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{{Name: "tenant-02-compute-9fjd4"}}, UpdatedAt: time.Now(),
	})

	f := fleetOf(a, b)
	got := f.managementEvents()

	var names []string
	for _, e := range got {
		names = append(names, e.ObjectName)
	}
	if len(got) != 1 || got[0].ObjectName != "draino-watcher-7c9fd8b64c" {
		t.Errorf("unclaimed events = %v, want only draino-watcher-7c9fd8b64c", names)
	}
}

// The management cluster's own events come from its own API server, so they are
// its own by definition and are never scoped away.
func TestManagementEvents_IncludesTheClustersOwnEvents(t *testing.T) {
	f := fleetOf(newTracked("machines", "tenant-01"))
	f.mgmt = &mgmtCollector{store: store.New(), cancel: func() {}}
	f.mgmt.store.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{
		Items: []model.Event{{
			Namespace: "kube-system", Type: "Warning", Reason: "FailedScheduling",
			ObjectKind: "Pod", ObjectName: "metallb-controller-1",
			Message: "no nodes available", LastTimestamp: time.Now(),
		}},
		UpdatedAt: time.Now(),
	})

	got := f.managementEvents()
	if len(got) != 1 || got[0].ObjectName != "metallb-controller-1" {
		t.Fatalf("got %d events, want the management cluster's own", len(got))
	}
}

// No management collector is not a reason to drop the Cluster API half: those
// events are read from a per-cluster store and are still unclaimed.
func TestManagementEvents_NoCollectorStillReportsUnclaimed(t *testing.T) {
	tr := newTracked("machines", "tenant-01")
	tr.store.Put(model.KeyMgmtEvents, model.Snapshot[model.Event]{
		Items:     []model.Event{mgmtEvent("draino-watcher-1", "PolicyViolation")},
		UpdatedAt: time.Now(),
	})

	f := fleetOf(tr)
	if f.mgmt != nil {
		t.Fatal("fixture should have no management collector")
	}
	if got := f.managementEvents(); len(got) != 1 {
		t.Errorf("got %d events, want the unclaimed one", len(got))
	}
}

// An unreachable management cluster reports the failure and nothing else: no
// nodes, no pods, no events invented out of an empty store.
func TestManagementDetail_UnreachableCarriesOnlyTheReason(t *testing.T) {
	f := fleetOf()
	f.mgmt = &mgmtCollector{store: store.New(), cancel: func() {}}
	unreachable := false
	f.mgmt.reachable = &unreachable

	d := f.ManagementDetail()
	if d.Reachable {
		t.Fatal("Reachable is true for an unreachable collector")
	}
	if d.NodeRows.Total() != 0 || len(d.UnhealthyPods) != 0 || d.Events.Total() != 0 {
		t.Error("reported data for a cluster that could not be read")
	}
}
