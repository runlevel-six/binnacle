package fleet

import (
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

// The shape grouping exists for: one admission policy rejecting many objects.
// Ungrouped this is forty rows that differ only in a hash; grouped it is one.
func TestGroupEvents_CollapsesManyObjects(t *testing.T) {
	now := time.Now()
	var raw []model.Event
	for i := 0; i < 40; i++ {
		raw = append(raw, model.Event{
			Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
			ObjectName:    "draino-watcher-" + itoa(i),
			Message:       "HostPath volumes are forbidden",
			Count:         2,
			LastTimestamp: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	got := GroupEvents(raw)
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Objects != 40 {
		t.Errorf("Objects = %d, want 40", got[0].Objects)
	}
	if got[0].Occurrences != 80 {
		t.Errorf("Occurrences = %d, want 80 (40 objects x count 2)", got[0].Occurrences)
	}
	// The representative is the newest member, so the age column reports when
	// this last happened rather than when it started.
	if got[0].ObjectName != "draino-watcher-0" {
		t.Errorf("representative = %q, want the newest member", got[0].ObjectName)
	}
}

// The message is part of the key. Two objects failing for different reasons are
// two problems, and collapsing them would hide one.
func TestGroupEvents_DifferentMessagesStaySeparate(t *testing.T) {
	now := time.Now()
	got := GroupEvents([]model.Event{
		{Type: "Warning", Reason: "Failed", ObjectKind: "Pod", ObjectName: "a",
			Message: "ImagePullBackOff", LastTimestamp: now},
		{Type: "Warning", Reason: "Failed", ObjectKind: "Pod", ObjectName: "b",
			Message: "CreateContainerConfigError", LastTimestamp: now},
	})
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	for _, g := range got {
		if g.Objects != 1 {
			t.Errorf("group %q: Objects = %d, want 1", g.Message, g.Objects)
		}
	}
}

// An event reporting no count still happened. Summing the raw field would
// report zero occurrences for a group that is plainly firing.
func TestGroupEvents_ZeroCountMeansOnce(t *testing.T) {
	now := time.Now()
	got := GroupEvents([]model.Event{
		{Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
			ObjectName: "a", Message: "forbidden", Count: 0, LastTimestamp: now},
		{Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
			ObjectName: "b", Message: "forbidden", Count: 0, LastTimestamp: now},
	})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Occurrences != 2 {
		t.Errorf("Occurrences = %d, want 2", got[0].Occurrences)
	}
}

// The same object reporting an event twice is one object, not two. Only the
// counts add up.
func TestGroupEvents_RepeatFromOneObject(t *testing.T) {
	now := time.Now()
	got := GroupEvents([]model.Event{
		{Type: "Warning", Reason: "BackOff", ObjectKind: "Pod", ObjectName: "api",
			Message: "restarting", Count: 5, LastTimestamp: now.Add(-time.Minute)},
		{Type: "Warning", Reason: "BackOff", ObjectKind: "Pod", ObjectName: "api",
			Message: "restarting", Count: 3, LastTimestamp: now},
	})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Objects != 1 {
		t.Errorf("Objects = %d, want 1", got[0].Objects)
	}
	if got[0].Occurrences != 8 {
		t.Errorf("Occurrences = %d, want 8", got[0].Occurrences)
	}
}

// Ordering survives grouping: warnings first, then most recent.
func TestGroupEvents_WarningsFirstThenRecent(t *testing.T) {
	now := time.Now()
	got := GroupEvents([]model.Event{
		{Type: "Normal", Reason: "Pulled", ObjectKind: "Pod", ObjectName: "a",
			Message: "pulled", LastTimestamp: now},
		{Type: "Warning", Reason: "Old", ObjectKind: "Pod", ObjectName: "b",
			Message: "old warning", LastTimestamp: now.Add(-time.Hour)},
		{Type: "Warning", Reason: "New", ObjectKind: "Pod", ObjectName: "c",
			Message: "new warning", LastTimestamp: now.Add(-time.Minute)},
	})
	want := []string{"new warning", "old warning", "pulled"}
	for i, w := range want {
		if got[i].Message != w {
			t.Errorf("group %d = %q, want %q", i, got[i].Message, w)
		}
	}
}

// Grouping runs before the cap, which is the whole point: sixty raw duplicates
// of one problem used to evict every other event on the page.
func TestSetEvents_CapAppliesToGroupsNotEvents(t *testing.T) {
	now := time.Now()
	var raw []model.Event
	// One noisy problem, reported by far more objects than the cap allows.
	for i := 0; i < maxEvents*3; i++ {
		raw = append(raw, model.Event{
			Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
			ObjectName: "noisy-" + itoa(i), Message: "forbidden",
			Count: 1, LastTimestamp: now.Add(-time.Hour),
		})
	}
	// The event that actually matters, oldest of the warnings so it would sort
	// last among them.
	raw = append(raw, model.Event{
		Type: "Warning", Reason: "FailedScheduling", ObjectKind: "Pod",
		ObjectName: "important", Message: "no nodes available",
		Count: 1, LastTimestamp: now.Add(-2 * time.Hour),
	})

	var d ClusterDetail
	d.setEvents(GroupEvents(raw))

	if d.Events.Total() != 2 {
		t.Fatalf("got %d groups, want 2", d.Events.Total())
	}
	if d.EventsTruncated != 0 {
		t.Errorf("EventsTruncated = %d, want 0", d.EventsTruncated)
	}
	if d.EventsTotal != maxEvents*3+1 {
		t.Errorf("EventsTotal = %d, want %d", d.EventsTotal, maxEvents*3+1)
	}
	var found bool
	for _, g := range d.Events.All() {
		if g.Message == "no nodes available" {
			found = true
		}
	}
	if !found {
		t.Error("the one event that mattered was evicted by duplicates of another")
	}
}

// When there really are more distinct problems than fit, the header has to be
// able to say so.
func TestSetEvents_TruncationCountsGroups(t *testing.T) {
	now := time.Now()
	var raw []model.Event
	for i := 0; i < maxEvents+5; i++ {
		raw = append(raw, model.Event{
			Type: "Warning", Reason: "Distinct", ObjectKind: "Pod",
			ObjectName: "pod-" + itoa(i), Message: "problem " + itoa(i),
			Count: 1, LastTimestamp: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	var d ClusterDetail
	d.setEvents(GroupEvents(raw))

	if d.Events.Total() != maxEvents {
		t.Errorf("shown = %d, want %d", d.Events.Total(), maxEvents)
	}
	if d.EventsTruncated != 5 {
		t.Errorf("EventsTruncated = %d, want 5", d.EventsTruncated)
	}
	if d.EventsTotal != maxEvents+5 {
		t.Errorf("EventsTotal = %d, want %d", d.EventsTotal, maxEvents+5)
	}
}

// The live regression this scoping exists for. Both underclouds in a
// datacenter share one management namespace, so k8s01's control plane being
// paused was reported on k8s00's page — where it reads as k8s00's own control
// plane being paused, which is the most alarming thing that page can say.
func TestEventsFor_AnotherClustersManagementEventIsNotOurs(t *testing.T) {
	var d ClusterDetail
	d.Name = "tenant-01-cluster"
	d.Pools = []NodePool{{Name: "tenant-01-kcp", Role: "control-plane"}}
	d.Machines = Split[model.Machine]{Shown: []model.Machine{
		{Name: "tenant-01-kcp-bfw9d", InfraName: "tenant-01-kcp-bfw9d"},
	}}

	mine, elsewhere := eventsFor([]model.Event{
		{Type: "Normal", Reason: "KCPPaused", ObjectKind: "KubeadmControlPlane",
			ObjectName: "tenant-02-kcp",
			Message:    "Paused tenant-02-kcp: StatefulSets not sufficiently ready"},
		{Type: "Normal", Reason: "KCPPaused", ObjectKind: "KubeadmControlPlane",
			ObjectName: "tenant-01-kcp",
			Message:    "Paused tenant-01-kcp: upgrade in progress"},
	}, d.ownedNames())

	if len(mine) != 1 {
		t.Fatalf("kept %d events, want 1", len(mine))
	}
	if got := mine[0].ObjectName; got != "tenant-01-kcp" {
		t.Errorf("kept %q, want this cluster's own control plane", got)
	}
	if elsewhere != 1 {
		t.Errorf("elsewhere = %d, want 1", elsewhere)
	}
}

// Cluster API names a pool's objects as extensions of each other, and only some
// of those names reach the page. An event about a MachineSet nobody listed is
// still this cluster's, and dropping it silently is worse than keeping it.
func TestEventsFor_KeepsObjectsNamedAroundAKnownName(t *testing.T) {
	var d ClusterDetail
	d.Name = "tenant-01-cluster"
	d.Machines = Split[model.Machine]{Shown: []model.Machine{
		{Name: "tenant-01-compute-ccqrw-f8tlh"},
	}}

	mine, elsewhere := eventsFor([]model.Event{
		// The MachineSet the machine came from: a prefix of a name we hold.
		{Reason: "SuccessfulCreate", ObjectKind: "MachineSet",
			ObjectName: "tenant-01-compute-ccqrw"},
		// A KubeadmConfig for that machine: an extension of a name we hold.
		{Reason: "Ready", ObjectKind: "KubeadmConfig",
			ObjectName: "tenant-01-compute-ccqrw-f8tlh-jt29v"},
		// Same shape, another cluster's pool.
		{Reason: "SuccessfulCreate", ObjectKind: "MachineSet",
			ObjectName: "tenant-02-compute-9wq2k"},
	}, d.ownedNames())

	if len(mine) != 2 {
		t.Fatalf("kept %d events, want 2", len(mine))
	}
	if elsewhere != 1 {
		t.Errorf("elsewhere = %d, want 1", elsewhere)
	}
}

// An empty owned set would otherwise drop every management event rather than
// none, which is the failure mode of reading events before the machines.
func TestEventsFor_NoOwnedNamesKeepsNothingAndSaysSo(t *testing.T) {
	mine, elsewhere := eventsFor([]model.Event{{ObjectName: "anything"}}, map[string]bool{})
	if len(mine) != 0 {
		t.Errorf("kept %d events, want 0", len(mine))
	}
	if elsewhere != 1 {
		t.Errorf("elsewhere = %d, want 1", elsewhere)
	}
}

// Warnings are the pane; Normal events are the audit trail behind it.
func TestSplitEvents_NormalGroupsFoldAndWarningsDoNot(t *testing.T) {
	now := time.Now()
	var raw []model.Event
	raw = append(raw, model.Event{Type: "Warning", Reason: "FailedScheduling",
		ObjectKind: "Pod", ObjectName: "api-1", Message: "no nodes available",
		LastTimestamp: now})
	// An etcd backup CronJob's steady output, which is what buries the warning.
	for i := 0; i < 12; i++ {
		raw = append(raw, model.Event{Type: "Normal", Reason: "Pulled",
			ObjectKind: "Pod", ObjectName: "etcd-backup-" + itoa(i),
			Message:       "Successfully pulled image busybox in " + itoa(i) + "ms",
			LastTimestamp: now.Add(-time.Duration(i) * time.Minute)})
	}

	var d ClusterDetail
	d.setEvents(GroupEvents(raw))

	if len(d.Events.Shown) != 1 {
		t.Fatalf("shown %d groups, want the one warning", len(d.Events.Shown))
	}
	if d.Events.Shown[0].Type != "Warning" {
		t.Errorf("shown group is %q, want Warning", d.Events.Shown[0].Type)
	}
	if len(d.Events.Quiet) != 12 {
		t.Errorf("folded %d groups, want 12", len(d.Events.Quiet))
	}
	if d.Events.Total() != 13 {
		t.Errorf("Total() = %d, want 13", d.Events.Total())
	}
}

// A dev cluster with a handful of Normal events reads better whole: hiding four
// rows behind a disclosure costs a click and saves nothing.
func TestSplitEvents_AFewNormalGroupsStayVisible(t *testing.T) {
	now := time.Now()
	var raw []model.Event
	for i := 0; i < 4; i++ {
		raw = append(raw, model.Event{Type: "Normal", Reason: "Pulled",
			ObjectKind: "Pod", ObjectName: "p-" + itoa(i), Message: "pulled " + itoa(i),
			LastTimestamp: now})
	}

	var d ClusterDetail
	d.setEvents(GroupEvents(raw))

	if len(d.Events.Quiet) != 0 {
		t.Errorf("folded %d groups on a quiet cluster, want none", len(d.Events.Quiet))
	}
	if len(d.Events.Shown) != 4 {
		t.Errorf("shown %d groups, want 4", len(d.Events.Shown))
	}
}

// Only the management side is namespace-wide. The workload cluster's events come
// from its own API server, so every one of them is already this cluster's — and
// scoping them by management object names would drop the lot.
func TestReadEvents_WorkloadEventsAreNotScoped(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{UpdatedAt: time.Now(),
		Items: []model.Event{{Namespace: "kube-system", Type: "Warning", Reason: "Unhealthy",
			ObjectKind: "Pod", ObjectName: "cilium-czwz7", Message: "probe failed",
			LastTimestamp: time.Now()}}})
	s.Put(model.KeyMgmtEvents, model.Snapshot[model.Event]{UpdatedAt: time.Now(),
		Items: []model.Event{{Type: "Normal", Reason: "KCPPaused",
			ObjectKind: "KubeadmControlPlane", ObjectName: "some-other-cluster-kcp",
			LastTimestamp: time.Now()}}})

	var d ClusterDetail
	d.Name = "ours"
	d.readEvents(s)

	if d.Events.Total() != 1 {
		t.Fatalf("kept %d groups, want the workload event", d.Events.Total())
	}
	if got := d.Events.All()[0].ObjectName; got != "cilium-czwz7" {
		t.Errorf("kept %q, want the workload event", got)
	}
	if d.EventsElsewhere != 1 {
		t.Errorf("EventsElsewhere = %d, want 1", d.EventsElsewhere)
	}
}

// Without the segment boundary a short owned name claims every object that
// merely starts with the same letters, which on a management cluster naming its
// clusters in a series is most of them.
func TestOwnsName_RequiresASegmentBoundary(t *testing.T) {
	owned := map[string]bool{"tenant-1": true}

	if ownsName(owned, "tenant-10-kcp") {
		t.Error("tenant-1 claimed an object belonging to tenant-10")
	}
	if !ownsName(owned, "tenant-1") {
		t.Error("an exact name is not owned")
	}
	if !ownsName(owned, "tenant-1-kcp") {
		t.Error("an extension of an owned name is not owned")
	}
}
