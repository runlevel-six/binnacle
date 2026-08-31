package fleet

import (
	"testing"

	"github.com/runlevel-six/sextant/pkg/model"
)

func node(name, role, status string, cordoned, pressure bool) NodeRow {
	n := model.Node{Name: name, Role: role, Status: status, Cordoned: cordoned}
	if pressure {
		n.MemoryPressure = true
	}
	return NodeRow{Node: n}
}

// A production undercloud is mostly healthy computes. The two rows worth
// reading have to be at the top, and the rest has to be foldable.
func TestSplitNodes_ProductionShape(t *testing.T) {
	var rows []NodeRow
	rows = append(rows, node("cp-1", "control-plane", "Ready", false, false))
	rows = append(rows, node("cp-2", "control-plane", "Ready", false, false))
	rows = append(rows, node("cp-3", "control-plane", "Ready", false, false))
	for i := 0; i < 60; i++ {
		rows = append(rows, node("compute-"+itoa(i), "compute", "Ready", false, false))
	}
	// The two that matter, deliberately last and alphabetically late so only
	// ranking can bring them forward.
	rows = append(rows, node("zz-down", "compute", "NotReady", false, false))
	rows = append(rows, node("zz-squeezed", "compute", "Ready", false, true))

	got := splitNodes(rows)
	if got.Total() != 65 {
		t.Fatalf("Total = %d, want 65", got.Total())
	}
	if len(got.Shown) != 2 {
		t.Fatalf("Shown = %d rows, want 2", len(got.Shown))
	}
	if got.Shown[0].Name != "zz-down" {
		t.Errorf("first shown = %q, want the NotReady node", got.Shown[0].Name)
	}
	if got.Shown[1].Name != "zz-squeezed" {
		t.Errorf("second shown = %q, want the node under pressure", got.Shown[1].Name)
	}
	if len(got.Quiet) != 63 {
		t.Errorf("Quiet = %d rows, want 63", len(got.Quiet))
	}
}

// A cordoned node is deliberate, not broken, but it still outranks a healthy
// one: a rollout that stalls mid-drain looks exactly like this.
func TestSplitNodes_CordonedOutranksHealthy(t *testing.T) {
	rows := []NodeRow{
		node("a-healthy", "compute", "Ready", false, false),
		node("z-cordoned", "compute", "Ready", true, false),
	}
	got := splitNodes(rows)
	if got.Shown[0].Name != "z-cordoned" {
		t.Errorf("first = %q, want the cordoned node", got.Shown[0].Name)
	}
}

// Below the threshold nothing folds: on a six-node cluster a disclosure costs a
// click and saves nothing.
func TestSplitNodes_SmallClusterDoesNotFold(t *testing.T) {
	var rows []NodeRow
	for i := 0; i < 6; i++ {
		rows = append(rows, node("node-"+itoa(i), "compute", "Ready", false, false))
	}
	got := splitNodes(rows)
	if len(got.Quiet) != 0 {
		t.Errorf("Quiet = %d, want 0 — six healthy nodes should render inline", len(got.Quiet))
	}
	if len(got.Shown) != 6 {
		t.Errorf("Shown = %d, want 6", len(got.Shown))
	}
}

// Ordering within a rank stays role-then-name, matching the pools table.
func TestSplitNodes_TiesKeepRoleThenName(t *testing.T) {
	rows := []NodeRow{
		node("b", "compute", "Ready", false, false),
		node("a", "compute", "Ready", false, false),
		node("z", "control-plane", "Ready", false, false),
	}
	got := splitNodes(rows)
	// Role then name, both alphabetical — "compute" sorts before "control-plane".
	want := []string{"a", "b", "z"}
	for i, w := range want {
		if got.Shown[i].Name != w {
			t.Errorf("row %d = %q, want %q", i, got.Shown[i].Name, w)
		}
	}
}

// Hosts are datacenter-wide, so this is the table that grows fastest. An
// errored host must survive to the top of a 250-row snapshot.
func TestSplitHosts_ErrorsSurviveTheFold(t *testing.T) {
	var rows []model.BareMetalHost
	for i := 0; i < 250; i++ {
		rows = append(rows, model.BareMetalHost{
			Name: "a-host-" + itoa(i), State: "provisioned", OperationalStatus: "OK",
		})
	}
	rows = append(rows, model.BareMetalHost{
		Name: "zz-broken", State: "provisioned", OperationalStatus: "error",
		ErrorMessage: "timed out waiting for the deploy image",
	})
	rows = append(rows, model.BareMetalHost{
		Name: "zz-detached", State: "provisioned", OperationalStatus: "detached",
	})

	got := splitHosts(rows)
	if len(got.Shown) != 2 {
		t.Fatalf("Shown = %d, want 2", len(got.Shown))
	}
	if got.Shown[0].Name != "zz-broken" {
		t.Errorf("first = %q, want the errored host", got.Shown[0].Name)
	}
	if got.Shown[1].Name != "zz-detached" {
		t.Errorf("second = %q, want the detached host", got.Shown[1].Name)
	}
	if len(got.Quiet) != 250 {
		t.Errorf("Quiet = %d, want 250", len(got.Quiet))
	}
}

// A host with no operational status reported is not thereby a problem.
func TestSplitHosts_EmptyStatusIsQuiet(t *testing.T) {
	var rows []model.BareMetalHost
	for i := 0; i < 12; i++ {
		rows = append(rows, model.BareMetalHost{Name: "h-" + itoa(i), State: "provisioned"})
	}
	got := splitHosts(rows)
	if len(got.Shown) != 0 {
		t.Errorf("Shown = %d, want 0", len(got.Shown))
	}
	if len(got.Quiet) != 12 {
		t.Errorf("Quiet = %d, want 12", len(got.Quiet))
	}
}

// Running is the only quiet phase; a stuck rollout and a healthy one look the
// same until someone reads the row.
func TestSplitMachines_PhaseOrdering(t *testing.T) {
	var rows []model.Machine
	for i := 0; i < 40; i++ {
		rows = append(rows, model.Machine{Name: "a-m-" + itoa(i), Phase: "Running"})
	}
	rows = append(rows,
		model.Machine{Name: "z-provisioning", Phase: "Provisioning"},
		model.Machine{Name: "z-deleting", Phase: "Deleting"},
		model.Machine{Name: "z-failed", Phase: "Failed"},
	)

	got := splitMachines(rows)
	want := []string{"z-failed", "z-deleting", "z-provisioning"}
	if len(got.Shown) != len(want) {
		t.Fatalf("Shown = %d, want %d", len(got.Shown), len(want))
	}
	for i, w := range want {
		if got.Shown[i].Name != w {
			t.Errorf("row %d = %q, want %q", i, got.Shown[i].Name, w)
		}
	}
	if len(got.Quiet) != 40 {
		t.Errorf("Quiet = %d, want 40", len(got.Quiet))
	}
}

// A machine reporting no phase at all is not thereby healthy.
func TestSplitMachines_EmptyPhaseRanksWorst(t *testing.T) {
	rows := []model.Machine{
		{Name: "a-running", Phase: "Running"},
		{Name: "z-silent"},
	}
	got := splitMachines(rows)
	if got.Shown[0].Name != "z-silent" {
		t.Errorf("first = %q, want the machine with no phase", got.Shown[0].Name)
	}
}

// The caller's slice must not be reordered underneath it. These come from the
// store, which hands the same backing array to every reader.
func TestSplit_DoesNotReorderTheCaller(t *testing.T) {
	rows := []model.Machine{
		{Name: "a-running", Phase: "Running"},
		{Name: "z-failed", Phase: "Failed"},
	}
	got := splitMachines(rows)
	if rows[0].Name != "a-running" || rows[1].Name != "z-failed" {
		t.Fatalf("the caller's slice was reordered: %q, %q", rows[0].Name, rows[1].Name)
	}
	// ...while the split itself is still ordered worst-first.
	if got.Shown[0].Name != "z-failed" {
		t.Errorf("Shown[0] = %q, want the failed machine", got.Shown[0].Name)
	}
}
