package openstack

import (
	"fmt"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/utils"
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

// Nova stamps updated_at when it happens to write the row, not when the
// migration ended, so a server's history does not arrive in updated_at order.
//
// This is the shape observed on a live cloud mid-drain: the first attempt's row
// was written fifteen minutes late, after every later attempt for that server
// had already finished. Ordering on updated_at picked it as current and rendered
// its source and destination, naming a pair of hosts the server had long since
// left — which is what the pane was reported as doing. The IDs and timestamps
// are the real ones; only the hostnames are substituted.
func TestLatestPerServerIgnoresOutOfOrderUpdatedAt(t *testing.T) {
	const server = "6f1c2b8e-4a3d-4f19-9c7e-2b8a5d1e0f34"
	host := func(n int) string { return fmt.Sprintf("compute-node-%d.site-a.example.com", n) }

	items := []Migration{
		// Created first, written last.
		{ID: 70551, InstanceUUID: server, Status: "completed",
			SourceCompute: host(1), DestCompute: host(2),
			CreatedAt: at(t, "2026-08-24T17:22:22Z"), UpdatedAt: at(t, "2026-08-24T17:37:47Z")},
		{ID: 70557, InstanceUUID: server, Status: "completed",
			SourceCompute: host(2), DestCompute: host(3),
			CreatedAt: at(t, "2026-08-24T17:23:52Z"), UpdatedAt: at(t, "2026-08-24T17:25:30Z")},
		{ID: 70602, InstanceUUID: server, Status: "completed",
			SourceCompute: host(3), DestCompute: host(4),
			CreatedAt: at(t, "2026-08-24T17:29:10Z"), UpdatedAt: at(t, "2026-08-24T17:30:03Z")},
		// Created last, and where the server actually ended up.
		{ID: 70608, InstanceUUID: server, Status: "completed",
			SourceCompute: host(4), DestCompute: host(5),
			CreatedAt: at(t, "2026-08-24T17:30:04Z"), UpdatedAt: at(t, "2026-08-24T17:31:03Z")},
	}

	got := LatestPerServer(items)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %v", len(got), ids(got))
	}
	if got[0].ID != 70608 {
		t.Errorf("kept id %d, want 70608 (the highest ID, not the latest updated_at)", got[0].ID)
	}
	// The symptom was the host pair, so assert on it and not only on the ID.
	if got[0].SourceCompute != host(4) || got[0].DestCompute != host(5) {
		t.Errorf("row names %s -> %s, want %s -> %s",
			got[0].SourceCompute, got[0].DestCompute, host(4), host(5))
	}
}

func TestLatestPerServerSortsFailedFirstThenNewest(t *testing.T) {
	items := []Migration{
		{ID: 1, InstanceUUID: "a", Status: "running", CreatedAt: at(t, "2026-07-28T10:05:00Z")},
		{ID: 2, InstanceUUID: "b", Status: "failed", CreatedAt: at(t, "2026-07-28T10:01:00Z")},
		{ID: 3, InstanceUUID: "c", Status: "running", CreatedAt: at(t, "2026-07-28T10:09:00Z")},
		{ID: 4, InstanceUUID: "d", Status: "error", CreatedAt: at(t, "2026-07-28T10:02:00Z")},
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
		{ID: 1, InstanceUUID: "a", Status: "running",
			CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-time.Minute)},
		// Old but still in flight: a migration stuck for a day is the single most
		// important row on the pane, so age must not evict it.
		{ID: 2, InstanceUUID: "b", Status: "queued",
			CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour)},
		{ID: 3, InstanceUUID: "c", Status: "failed",
			CreatedAt: now.Add(-95 * time.Minute), UpdatedAt: now.Add(-90 * time.Minute)},
		{ID: 4, InstanceUUID: "d", Status: "failed",
			CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: 5, InstanceUUID: "e", Status: "completed",
			CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-time.Minute)},
	}

	got := ids(Migrations{Items: items}.Relevant(now).Rows)
	// Relevant sorts through LatestPerServer now, so this is failures first and
	// then the rest newest-first, rather than the input order it used to keep.
	want := []int64{3, 1, 2}
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
	inside := Migrations{Items: []Migration{
		{ID: 1, Status: "failed", UpdatedAt: now.Add(-FailedWindow)}}}
	outside := Migrations{Items: []Migration{
		{ID: 2, Status: "failed", UpdatedAt: now.Add(-FailedWindow - time.Second)}}}

	if got := inside.Relevant(now); len(got.Rows) != 1 {
		t.Errorf("failure exactly at the window was dropped")
	}
	if got := outside.Relevant(now); len(got.Rows) != 0 {
		t.Errorf("failure past the window was kept")
	}
}

// The retention rule, as a matrix. An unresolved failure is never expired on
// age; what varies is whether it takes a row or only a place in the count.
func TestRelevantRetainsUnresolvedFailures(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const (
		drained = "compute-node-1.site-a.example.com"
		quiet   = "compute-node-2.site-a.example.com"
	)
	old := now.Add(-72 * time.Hour) // older than any upgrade window

	// Two failures from a previous upgrade, both instances still in ERROR. One
	// sits on a host being drained right now, the other does not.
	items := []Migration{
		{ID: 1, InstanceUUID: "blocking", Status: "error", UpdatedAt: old},
		{ID: 2, InstanceUUID: "backlog", Status: "error", UpdatedAt: old},
		// Still broken but healthy again in Nova's eyes: nothing retains it.
		{ID: 3, InstanceUUID: "recovered", Status: "error", UpdatedAt: old},
	}
	snap := Migrations{
		Items:       items,
		BrokenKnown: true,
		Broken: map[string]BrokenServer{
			"blocking": {UUID: "blocking", Host: drained},
			"backlog":  {UUID: "backlog", Host: quiet},
		},
		Draining: map[string]bool{drained: true},
	}

	got := snap.Relevant(now)
	if ids := ids(got.Rows); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("listed %v, want only id 1 — the one on a host being drained", ids)
	}
	if ids := ids(got.Unresolved); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("counted %v, want only id 2 — retained but not in the way", ids)
	}
}

// A probe that could not run must never hide a row. Without it the pane falls
// back to the age window alone, which is the behavior that shipped before.
func TestRelevantFallsBackWhenProbeUnavailable(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	items := []Migration{
		{ID: 1, InstanceUUID: "a", Status: "error", UpdatedAt: now.Add(-time.Minute)},
		{ID: 2, InstanceUUID: "b", Status: "error", UpdatedAt: now.Add(-72 * time.Hour)},
	}

	// BrokenKnown false, and Broken deliberately populated to prove it is not
	// consulted: a probe that failed leaves whatever the last one found, and
	// acting on it would promote rows from a stale answer.
	snap := Migrations{
		Items:  items,
		Broken: map[string]BrokenServer{"b": {UUID: "b", Host: "h"}},
	}

	got := snap.Relevant(now)
	if ids := ids(got.Rows); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("listed %v, want only the recent failure id 1", ids)
	}
	if len(got.Unresolved) != 0 {
		t.Errorf("counted %v as unresolved without a usable probe", ids(got.Unresolved))
	}
}

// A recent failure is listed whether or not its instance recovered, because a
// migration that failed is a server that did not leave the host.
func TestRelevantListsRecentFailuresRegardlessOfInstanceState(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	snap := Migrations{
		Items: []Migration{
			{ID: 1, InstanceUUID: "healthy", Status: "failed", UpdatedAt: now.Add(-time.Minute)},
		},
		BrokenKnown: true,
		Broken:      map[string]BrokenServer{},
	}

	got := snap.Relevant(now)
	if len(got.Rows) != 1 {
		t.Errorf("listed %v, want the recent failure even though the instance is fine", ids(got.Rows))
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

// The limit parameter must go out only where Nova honors it. Below 2.59 the
// endpoint validates no query parameters, so an unsupported limit is silently
// ignored — the request would look bounded and would not be.
func TestSupportsMigrationPaging(t *testing.T) {
	tests := map[string]bool{
		"2.59": true,
		"2.60": true,
		"2.80": true,
		"3.0":  true,
		"2.23": false,
		"2.1":  false,
		// No microversion negotiated: the request goes out at Nova's minimum.
		"":        false,
		"garbage": false,
	}
	for in, want := range tests {
		if got := supportsMigrationPaging(in); got != want {
			t.Errorf("supportsMigrationPaging(%q) = %v, want %v", in, got, want)
		}
	}
}

// Every version this poll is willing to ask for must be one Nova actually
// changed /os-migrations at, and they must be ordered best-first.
func TestMigrationMicroversionsAreOrderedBestFirst(t *testing.T) {
	if len(migrationMicroversions) == 0 {
		t.Fatal("no microversion is requested, so Nova serves 2.1 and strips migration_type")
	}
	prevMajor, prevMinor := 0, 0
	for i, v := range migrationMicroversions {
		major, minor, err := utils.ParseMicroversion(v)
		if err != nil {
			t.Fatalf("migrationMicroversions[%d] = %q: %v", i, v, err)
		}
		if i > 0 && (major > prevMajor || (major == prevMajor && minor >= prevMinor)) {
			t.Errorf("migrationMicroversions[%d] = %q does not descend from the previous entry", i, v)
		}
		prevMajor, prevMinor = major, minor
	}
	// 2.23 is where migration_type appears; asking for anything below it would
	// leave the type column blank, which is the bug this list exists to fix.
	last := migrationMicroversions[len(migrationMicroversions)-1]
	major, minor, _ := utils.ParseMicroversion(last)
	if major < 2 || (major == 2 && minor < 23) {
		t.Errorf("lowest requested microversion %q is below 2.23, so migration_type is stripped", last)
	}
}

func ids(items []Migration) []int64 {
	out := make([]int64, 0, len(items))
	for _, m := range items {
		out = append(out, m.ID)
	}
	return out
}
