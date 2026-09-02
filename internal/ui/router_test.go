package ui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/demo"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/testansi"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// testBuilder builds a cluster dashboard model from the demo fixture.
// It mirrors what app.BuildModel does, without importing the app package
// (which would be a circular dependency).
func testBuilder(_, _ string) (*Model, func(), error) {
	theme := tui.DefaultTheme()
	tui.ApplyTheme(theme)
	resolved := demo.Resolved(theme)
	store := demo.Store()
	reg := plugin.NewRegistry()
	for _, p := range demo.Plugins() {
		reg.Register(p)
	}
	reg.Detect(context.Background())
	panes := CorePanes(store, resolved, nil)
	pluginPanes, _ := tui.Panes(reg, store, []tui.PaneProvider{})
	all := tui.Group(append(panes, pluginPanes...))
	m := New(resolved, store, reg, all).
		WithBuild(build.Info{Version: "test", Commit: "abc", Date: "2026-09-01T00:00:00Z"})
	return m, nil, nil
}

func newSextant(w, h int) *SextantModel {
	demo := fleet.NewDemo()
	fm := NewFleet(demo, "demo", build.Info{Version: "test", Commit: "abc", Date: "2026-09-01T00:00:00Z"})
	fm.Update(tea.WindowSizeMsg{Width: w, Height: h})
	fm.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	fm.SetBuilder(testBuilder)
	router := NewSextant(fm, testBuilder, build.Info{Version: "test", Commit: "abc", Date: "2026-09-01T00:00:00Z"})
	router.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return router
}

// drillDown presses Enter and then feeds the resulting DrillDownMsg back
// into the router, simulating what the Bubble Tea runtime does.
func drillDown(r *SextantModel) {
	_, cmd := r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		r.Update(cmd())
	}
}

// pressKey sends a key and feeds back any resulting message.
func pressKey(r *SextantModel, msg tea.Msg) {
	_, cmd := r.Update(msg)
	if cmd != nil {
		r.Update(cmd())
	}
}

func TestRouter_StartsOnFleetScreen(t *testing.T) {
	r := newSextant(200, 50)
	if r.screen != "fleet" {
		t.Errorf("should start on fleet screen, got %q", r.screen)
	}
	view := testansi.StripANSI(r.View())
	if !strings.Contains(view, "CLUSTER") {
		t.Error("fleet screen should show the cluster list")
	}
}

func TestRouter_EnterDrillsIntoCluster(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)

	if r.screen != "cluster" {
		t.Errorf("after enter, should be on cluster screen, got %q", r.screen)
	}
	if r.cluster == nil {
		t.Fatal("cluster model should be built")
	}
	view := testansi.StripANSI(r.View())
	if !strings.Contains(view, "sextant") {
		t.Error("cluster screen should render the dashboard")
	}
}

func TestRouter_EscReturnsToFleet(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)
	if r.screen != "cluster" {
		t.Fatal("should be on cluster screen")
	}
	r.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if r.screen != "fleet" {
		t.Errorf("after esc, should be back on fleet, got %q", r.screen)
	}
	if r.cluster != nil {
		t.Error("cluster model should be nil after esc")
	}
}

func TestRouter_QuitFromFleet(t *testing.T) {
	r := newSextant(200, 50)
	_, cmd := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q from fleet should quit")
	}
}

func TestRouter_QuitFromCluster(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)
	if r.screen != "cluster" {
		t.Fatal("should be on cluster screen before pressing q")
	}
	_, cmd := r.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q from cluster should quit")
	}
}

func TestRouter_FleetCursorPreservedAfterReturn(t *testing.T) {
	r := newSextant(200, 50)
	pressKey(r, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	pressKey(r, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	before := r.fleet.selected

	drillDown(r)
	r.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if r.fleet.selected != before {
		t.Errorf("cursor should be preserved: got %d want %d", r.fleet.selected, before)
	}
}

func TestRouter_WindowSizeForwardedToFleet(t *testing.T) {
	r := newSextant(200, 50)
	r.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if r.fleet.width != 120 || r.fleet.height != 30 {
		t.Errorf("fleet should get window size: got %dx%d", r.fleet.width, r.fleet.height)
	}
}

func TestRouter_WindowSizeForwardedToCluster(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)
	r.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if r.cluster.width != 120 || r.cluster.height != 30 {
		t.Errorf("cluster should get window size: got %dx%d", r.cluster.width, r.cluster.height)
	}
}

func TestRouter_DrillDownMsgTransitions(t *testing.T) {
	r := newSextant(200, 50)
	r.Update(DrillDownMsg{Namespace: "managed-clusters", Name: "tenant-01"})
	if r.screen != "cluster" {
		t.Errorf("DrillDownMsg should switch to cluster screen, got %q", r.screen)
	}
}

func TestRouter_RepeatedDrillDown(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)
	r.Update(tea.KeyMsg{Type: tea.KeyEsc})
	drillDown(r)
	if r.screen != "cluster" {
		t.Errorf("second drill-down should work, got %q", r.screen)
	}
	if r.cluster == nil {
		t.Fatal("cluster model should be built on second drill-down")
	}
}

func TestRouter_FleetMessagesForwarded(t *testing.T) {
	r := newSextant(200, 50)
	demo := fleet.NewDemo()
	r.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	if len(r.fleet.clusters) == 0 {
		t.Error("fleet update should reach the fleet model")
	}
}

func TestRouter_ClusterKeysForwarded(t *testing.T) {
	r := newSextant(200, 50)
	drillDown(r)
	before := r.cluster.focused
	r.Update(tea.KeyMsg{Type: tea.KeyTab})
	if r.cluster.focused == before {
		t.Error("tab should be forwarded to cluster model")
	}
}

func TestRouter_ViewEmptyBeforeSizeKnown(t *testing.T) {
	fm := NewFleet(fleet.NewDemo(), "demo", build.Info{})
	fm.SetBuilder(testBuilder)
	r := NewSextant(fm, testBuilder, build.Info{})
	if got := r.View(); got != "" {
		t.Errorf("expected empty view before size known, got %q", got)
	}
}

func TestRouter_FleetEnterEmitsDrillDownWhenBuilderSet(t *testing.T) {
	r := newSextant(200, 50)
	_, cmd := r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return a command")
	}
	msg := cmd()
	if _, ok := msg.(DrillDownMsg); !ok {
		t.Errorf("enter should emit DrillDownMsg, got %T", msg)
	}
}

func TestRouter_FleetEnterFallsBackWithoutBuilder(t *testing.T) {
	demo := fleet.NewDemo()
	fm := NewFleet(demo, "demo", build.Info{})
	fm.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	fm.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	_, cmd := fm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return a command even without builder")
	}
	msg := cmd()
	if _, ok := msg.(clusterDetailMsg); !ok {
		t.Errorf("without builder, enter should fetch text detail, got %T", msg)
	}
}
