package fleet

import (
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
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

	if len(d.Events) != 2 {
		t.Fatalf("got %d groups, want 2", len(d.Events))
	}
	if d.EventsTruncated != 0 {
		t.Errorf("EventsTruncated = %d, want 0", d.EventsTruncated)
	}
	if d.EventsTotal != maxEvents*3+1 {
		t.Errorf("EventsTotal = %d, want %d", d.EventsTotal, maxEvents*3+1)
	}
	var found bool
	for _, g := range d.Events {
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

	if len(d.Events) != maxEvents {
		t.Errorf("shown = %d, want %d", len(d.Events), maxEvents)
	}
	if d.EventsTruncated != 5 {
		t.Errorf("EventsTruncated = %d, want 5", d.EventsTruncated)
	}
	if d.EventsTotal != maxEvents+5 {
		t.Errorf("EventsTotal = %d, want %d", d.EventsTotal, maxEvents+5)
	}
}
