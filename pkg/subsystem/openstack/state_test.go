package openstack

import "testing"

func TestErrorState(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		// One vocabulary per service, which is why this matches a substring.
		{"ERROR", true},           // a server, or a load balancer's provisioning status
		{"ERROR_DELETING", true},  // a volume Cinder could not remove
		{"ERROR_RESTORING", true}, // and one it could not restore
		{"error", true},           // lower case, in case a service reports it that way
		{"FAILED", true},
		{"ACTIVE", false},
		{"AVAILABLE", false},
		{"IN-USE", false},
		{"FREE", false},
		// Transitional, and deliberately not a failure: a cloud of any size
		// always has a few, and a standing warning is no warning.
		{"BUILD", false},
		{"PENDING_UPDATE", false},
		{"ATTACHING", false},
		{"RESERVED", false},
	} {
		if got := ErrorState(tc.state); got != tc.want {
			t.Errorf("ErrorState(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// Both front ends read the same breakdown, so the order cannot come from
// iterating the map.
func TestStateCounts_MostCommonFirstThenAlphabetical(t *testing.T) {
	got := StateCounts(map[string]int{
		"IN-USE": 71, "AVAILABLE": 39, "ERROR_DELETING": 11,
		"RESERVED": 15, "ATTACHING": 1, "DETACHING": 1,
	})

	want := []StateCount{
		{State: "IN-USE", Count: 71},
		{State: "AVAILABLE", Count: 39},
		{State: "RESERVED", Count: 15},
		{State: "ERROR_DELETING", Count: 11, Error: true},
		// The tie is broken alphabetically, so the order is stable between
		// renders rather than whatever the map handed out.
		{State: "ATTACHING", Count: 1},
		{State: "DETACHING", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An empty breakdown is not an error, and a nil map is what a kind with no
// per-state detail carries.
func TestStateCounts_EmptyIsEmpty(t *testing.T) {
	if got := StateCounts(nil); len(got) != 0 {
		t.Errorf("StateCounts(nil) = %+v, want empty", got)
	}
}
