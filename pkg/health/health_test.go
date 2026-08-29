package health

import (
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

// The Nodes cell must not sit at amber for the life of a cluster whose
// hypervisors are cordoned on purpose. The severity is the whole point of a
// health indicator: a permanent warning is indistinguishable from no warning.
func TestCellNodes_ExpectedCordonStaysGreen(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "cp-1", Status: "Ready", Role: "control-plane"},
			{Name: "compute-1", Status: "Ready", Role: "compute", Cordoned: true},
			{Name: "compute-2", Status: "Ready", Role: "compute", Cordoned: true},
		},
		UpdatedAt: time.Now(),
	})

	roles := profile.NodeRoles{CordonExpected: []string{"compute"}}
	cell, ok := cellNodes(s, roles)
	if !ok {
		t.Fatal("no nodes cell")
	}
	if cell.Status != StatusOK {
		t.Errorf("status = %v (%q), want OK", cell.Status, cell.Detail)
	}

	// Without the setting the same fleet warns, which is right on a stock
	// cluster where a cordon really is a drain.
	if cell, _ := cellNodes(s, profile.NodeRoles{}); cell.Status != StatusWarn {
		t.Errorf("unconfigured status = %v, want Warn", cell.Status)
	}
}

// An exemption must not swallow a real failure on an exempt node.
func TestCellNodes_ExpectedCordonStillReportsNotReady(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "compute-1", Status: "NotReady", Role: "compute", Cordoned: true},
		},
		UpdatedAt: time.Now(),
	})

	cell, _ := cellNodes(s, profile.NodeRoles{CordonExpected: []string{"compute"}})
	if cell.Status != StatusErr {
		t.Errorf("status = %v (%q), want Err", cell.Status, cell.Detail)
	}
}

// A store nothing has published into contributes no cells at all, rather than a
// row of green ones. The distinction matters most on a fleet page, where a
// cluster that has not been heard from must not be summarised as healthy.
func TestCoreCells_EmptyStoreReportsNothing(t *testing.T) {
	if got := CoreCells(store.New(), profile.NodeRoles{}); len(got) != 0 {
		t.Errorf("got %d cells from an empty store, want none", len(got))
	}
}

// Worst is the fleet page's fold. Loading is the zero value so that an
// unreported cluster stays unknown; anything worse than what we have seen so
// far wins.
func TestWorst(t *testing.T) {
	for name, tc := range map[string]struct {
		cells []Cell
		want  Status
	}{
		"nothing":      {nil, StatusLoading},
		"all healthy":  {[]Cell{{Status: StatusOK}, {Status: StatusOK}}, StatusOK},
		"one degraded": {[]Cell{{Status: StatusOK}, {Status: StatusWarn}}, StatusWarn},
		"one broken":   {[]Cell{{Status: StatusWarn}, {Status: StatusErr}, {Status: StatusOK}}, StatusErr},
		// A cell still loading must not drag a healthy fleet backwards, but it
		// must not mask a broken one either.
		"partial": {[]Cell{{Status: StatusLoading}, {Status: StatusOK}}, StatusOK},
	} {
		if got := Worst(tc.cells); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

// The status name is used as a CSS class and in markup, so it is API, not a
// debugging convenience.
func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{
		StatusLoading: "loading", StatusOK: "ok", StatusWarn: "warn", StatusErr: "err",
	} {
		if got := s.String(); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}
