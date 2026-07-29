package openstack

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return got
}

func TestActiveAndFailedStatuses(t *testing.T) {
	tests := []struct {
		status         string
		active, failed bool
	}{
		{"queued", true, false},
		{"preparing", true, false},
		{"accepted", true, false},
		{"pre-migrating", true, false},
		{"migrating", true, false},
		{"running", true, false},
		{"post-migrating", true, false},
		{"RUNNING", true, false}, // Nova has shipped both cases over the years
		{"failed", false, true},
		{"error", false, true},
		{"Error", false, true},
		// Terminal successes and anything unrecognized are neither, so they
		// leave the pane rather than sitting there forever.
		{"completed", false, false},
		{"done", false, false},
		{"canceled", false, false},
		{"reverted", false, false},
		{"some-future-state", false, false},
		{"", false, false},
	}

	for _, tc := range tests {
		if got := Active(tc.status); got != tc.active {
			t.Errorf("Active(%q) = %v, want %v", tc.status, got, tc.active)
		}
		if got := Failed(tc.status); got != tc.failed {
			t.Errorf("Failed(%q) = %v, want %v", tc.status, got, tc.failed)
		}
	}
}

// A retry sequence for one server is one story, and must not occupy four rows
// during the upgrade where every server is moving.
func TestLatestPerServerCollapsesRetries(t *testing.T) {
	const server = "11111111-2222-3333-4444-555555555555"
	items := []Migration{
		{ID: 1, InstanceUUID: server, Status: "queued", UpdatedAt: at(t, "2026-07-28T10:00:00Z")},
		{ID: 2, InstanceUUID: server, Status: "running", UpdatedAt: at(t, "2026-07-28T10:01:00Z")},
		{ID: 3, InstanceUUID: server, Status: "failed", UpdatedAt: at(t, "2026-07-28T10:02:00Z")},
		{ID: 4, InstanceUUID: server, Status: "running", UpdatedAt: at(t, "2026-07-28T10:03:00Z")},
	}

	got := LatestPerServer(items)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ID != 4 || got[0].Status != "running" {
		t.Errorf("kept id %d (%s), want the newest attempt id 4 (running)", got[0].ID, got[0].Status)
	}
}

// Nova's updated_at resolution is coarser than a retry loop, so equal timestamps
// are ordinary and the tie must resolve the same way every render.
func TestLatestPerServerBreaksTiesOnID(t *testing.T) {
	const server = "aaaa"
	same := at(t, "2026-07-28T10:00:00Z")
	items := []Migration{
		{ID: 7, InstanceUUID: server, Status: "queued", UpdatedAt: same},
		{ID: 9, InstanceUUID: server, Status: "running", UpdatedAt: same},
		{ID: 8, InstanceUUID: server, Status: "failed", UpdatedAt: same},
	}

	for range 5 {
		got := LatestPerServer(items)
		if len(got) != 1 || got[0].ID != 9 {
			t.Fatalf("got %+v, want only id 9", got)
		}
	}
}

func TestLatestPerServerSortsFailedFirstThenNewest(t *testing.T) {
	items := []Migration{
		{ID: 1, InstanceUUID: "a", Status: "running", UpdatedAt: at(t, "2026-07-28T10:05:00Z")},
		{ID: 2, InstanceUUID: "b", Status: "failed", UpdatedAt: at(t, "2026-07-28T10:01:00Z")},
		{ID: 3, InstanceUUID: "c", Status: "running", UpdatedAt: at(t, "2026-07-28T10:09:00Z")},
		{ID: 4, InstanceUUID: "d", Status: "error", UpdatedAt: at(t, "2026-07-28T10:02:00Z")},
	}

	got := LatestPerServer(items)
	want := []int64{4, 2, 3, 1} // failures newest-first, then the rest newest-first
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("row %d is id %d, want %d (order: %v)", i, got[i].ID, id, ids(got))
		}
	}
}

// A record with no instance UUID should not silently swallow its neighbors by
// colliding on the empty key.
func TestLatestPerServerKeepsUUIDLessRecordsSeparate(t *testing.T) {
	items := []Migration{
		{ID: 1, Status: "running", UpdatedAt: at(t, "2026-07-28T10:00:00Z")},
		{ID: 2, Status: "running", UpdatedAt: at(t, "2026-07-28T10:01:00Z")},
	}
	if got := LatestPerServer(items); len(got) != 2 {
		t.Errorf("got %d rows, want 2: %v", len(got), ids(got))
	}
}

func TestRelevantKeepsActiveAndRecentFailures(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	items := []Migration{
		{ID: 1, InstanceUUID: "a", Status: "running", UpdatedAt: now.Add(-time.Minute)},
		// Old but still in flight: a migration stuck for a day is the single most
		// important row on the pane, so age must not evict it.
		{ID: 2, InstanceUUID: "b", Status: "queued", UpdatedAt: now.Add(-24 * time.Hour)},
		{ID: 3, InstanceUUID: "c", Status: "failed", UpdatedAt: now.Add(-90 * time.Minute)},
		{ID: 4, InstanceUUID: "d", Status: "failed", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: 5, InstanceUUID: "e", Status: "completed", UpdatedAt: now.Add(-time.Minute)},
	}

	got := ids(Relevant(items, now))
	want := []int64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kept %v, want %v", got, want)
		}
	}
}

func TestRelevantFailureWindowBoundary(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	// Exactly at the window is kept; a second past it is not.
	inside := []Migration{{ID: 1, Status: "failed", UpdatedAt: now.Add(-FailedWindow)}}
	outside := []Migration{{ID: 2, Status: "failed", UpdatedAt: now.Add(-FailedWindow - time.Second)}}

	if got := Relevant(inside, now); len(got) != 1 {
		t.Errorf("failure exactly at the window was dropped")
	}
	if got := Relevant(outside, now); len(got) != 0 {
		t.Errorf("failure past the window was kept")
	}
}

// Nova's wire format has no timezone and Nova emits UTC; the RFC3339 variants
// are accepted in case a later microversion changes shape.
func TestParseNovaTime(t *testing.T) {
	want := at(t, "2026-07-28T10:11:12Z")
	for _, in := range []string{
		"2026-07-28T10:11:12.000000",
		"2026-07-28T10:11:12",
		"2026-07-28T10:11:12Z",
		"2026-07-28T10:11:12.000Z",
	} {
		if got := parseNovaTime(in); !got.Equal(want) {
			t.Errorf("parseNovaTime(%q) = %v, want %v", in, got, want)
		}
	}

	// An unreadable timestamp must not cost the row: zero reads as unknown age.
	for _, in := range []string{"", "not-a-time", "28/07/2026"} {
		if got := parseNovaTime(in); !got.IsZero() {
			t.Errorf("parseNovaTime(%q) = %v, want the zero time", in, got)
		}
	}
}

func ids(items []Migration) []int64 {
	out := make([]int64, 0, len(items))
	for _, m := range items {
		out = append(out, m.ID)
	}
	return out
}
