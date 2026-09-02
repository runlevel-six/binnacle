package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/testansi"
	"github.com/runlevel-six/binnacle/pkg/health"
)

func newFleetModel(w, h int) *FleetModel {
	demo := fleet.NewDemo()
	m := NewFleet(demo, "demo", build.Info{Version: "test", Commit: "abc", Date: "2026-09-01T00:00:00Z"})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	return m
}

func TestFleetView_EmptyBeforeSizeKnown(t *testing.T) {
	m := NewFleet(fleet.NewDemo(), "demo", build.Info{})
	if got := m.View(); got != "" {
		t.Errorf("expected empty view before size is known, got %q", got)
	}
}

func TestFleetView_ExactDimensions(t *testing.T) {
	for _, size := range []struct{ w, h int }{
		{80, 24}, {120, 40}, {200, 50},
	} {
		m := newFleetModel(size.w, size.h)
		view := m.View()
		if got := lipgloss.Height(view); got != size.h {
			t.Errorf("%dx%d: height %d want %d", size.w, size.h, got, size.h)
		}
		for i, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > size.w {
				t.Errorf("%dx%d: line %d width %d exceeds terminal", size.w, size.h, i, got)
			}
		}
	}
}

func TestFleetView_NamesClusters(t *testing.T) {
	m := newFleetModel(200, 50)
	view := testansi.StripANSI(m.View())
	for _, want := range []string{"tenant-01", "tenant-02-cluster", "tenant-03-cluster"} {
		if !strings.Contains(view, want) {
			t.Errorf("fleet view should name %q:\n%s", want, firstLines(view, 10))
		}
	}
}

func TestFleetView_HeaderNamesServer(t *testing.T) {
	m := newFleetModel(200, 50)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "sextant") {
		t.Error("header should identify the tool")
	}
	if !strings.Contains(view, "demo") {
		t.Error("header should name the server URL")
	}
}

func TestFleetSort_WorstFirst(t *testing.T) {
	m := newFleetModel(200, 50)
	// Worst-first means Err clusters come before Warn, which come before OK.
	var prevStatus health.Status
	for i, c := range m.clusters {
		if i == 0 {
			prevStatus = c.Status
			continue
		}
		if c.Status > prevStatus {
			t.Errorf("cluster %d (%s, status %s) is worse than cluster %d (status %s); should be worst-first",
				i, c.Name, c.Status, i-1, prevStatus)
		}
		prevStatus = c.Status
	}
}

func TestFleetSort_Reverse(t *testing.T) {
	m := newFleetModel(200, 50)
	before := make([]string, len(m.clusters))
	for i, c := range m.clusters {
		before[i] = c.Name
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})

	// After reverse, the order should be flipped.
	for i, c := range m.clusters {
		want := before[len(before)-1-i]
		if c.Name != want {
			t.Errorf("after reverse, cluster %d got %q want %q", i, c.Name, want)
		}
	}
	// Worst-first should now be worst-last: OK clusters lead.
	if len(m.clusters) > 0 {
		if m.clusters[0].Status == health.StatusErr {
			t.Error("after reverse, the first cluster should not be the worst one")
		}
	}
}

func TestFleetSort_ReverseToggle(t *testing.T) {
	m := newFleetModel(200, 50)
	original := make([]string, len(m.clusters))
	for i, c := range m.clusters {
		original[i] = c.Name
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	for i, c := range m.clusters {
		if c.Name != original[i] {
			t.Errorf("double-reverse should restore order: cluster %d got %q want %q", i, c.Name, original[i])
		}
	}
}

func TestFleetKeys_NavigateDown(t *testing.T) {
	m := newFleetModel(200, 50)
	first := m.selected
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.selected != first+1 {
		t.Errorf("j should move down: got %d want %d", m.selected, first+1)
	}
}

func TestFleetKeys_NavigateUp(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	before := m.selected
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.selected != before-1 {
		t.Errorf("k should move up: got %d want %d", m.selected, before-1)
	}
}

func TestFleetKeys_UpClampedAtTop(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.selected != 0 {
		t.Errorf("k at top should stay at 0: got %d", m.selected)
	}
}

func TestFleetKeys_DownClampedAtBottom(t *testing.T) {
	m := newFleetModel(200, 50)
	for range len(m.clusters) + 5 {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	if m.selected != len(m.clusters)-1 {
		t.Errorf("j past bottom should clamp: got %d want %d", m.selected, len(m.clusters)-1)
	}
}

func TestFleetKeys_Quit(t *testing.T) {
	m := newFleetModel(200, 50)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

func TestFleetKeys_EnterDrillsIntoDetail(t *testing.T) {
	m := newFleetModel(200, 50)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return a command to fetch cluster detail")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("enter should produce a message")
	}
	if _, ok := msg.(clusterDetailMsg); !ok {
		t.Errorf("enter should produce a clusterDetailMsg, got %T", msg)
	}
}

func TestFleetDetail_ShowsClusterName(t *testing.T) {
	m := newFleetModel(200, 50)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return a command")
	}
	msg := cmd()
	m.Update(msg)
	view := testansi.StripANSI(m.View())
	if m.viewing != "detail" {
		t.Fatalf("should be in detail view, got %q", m.viewing)
	}
	selected := m.clusters[m.selected]
	if !strings.Contains(view, selected.Name) {
		t.Errorf("detail view should name the selected cluster %q:\n%s", selected.Name, firstLines(view, 5))
	}
}

func TestFleetDetail_EscReturnsToFleet(t *testing.T) {
	m := newFleetModel(200, 50)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return a command")
	}
	m.Update(cmd())
	if m.viewing != "detail" {
		t.Fatal("should be in detail view")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.viewing != "fleet" {
		t.Error("esc should return to fleet view")
	}
	if m.detail != nil {
		t.Error("detail should be cleared after esc")
	}
}

func TestFleetAutoSelect_DrillsIntoCluster(t *testing.T) {
	demo := fleet.NewDemo()
	m := NewFleet(demo, "demo", build.Info{})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})

	// Set auto-select to the first cluster in the fixture.
	m.SetAutoSelect("managed-clusters/tenant-01")

	_, cmd := m.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	if cmd == nil {
		t.Fatal("auto-select should produce a command to fetch cluster detail")
	}
	msg := cmd()
	if _, ok := msg.(clusterDetailMsg); !ok {
		t.Errorf("auto-select should produce a clusterDetailMsg, got %T", msg)
	}
}

func TestFleetAutoSelect_UnmatchedStaysInFleet(t *testing.T) {
	demo := fleet.NewDemo()
	m := NewFleet(demo, "demo", build.Info{})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})

	m.SetAutoSelect("managed-clusters/nonexistent")

	_, cmd := m.Update(FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	if cmd != nil {
		t.Error("auto-select for a nonexistent cluster should not produce a command")
	}
	if m.viewing != "fleet" {
		t.Errorf("should stay in fleet view, got %q", m.viewing)
	}
}

func TestFleetFooter_MentionsReverseKey(t *testing.T) {
	m := newFleetModel(200, 50)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "r reverse") {
		t.Error("footer should mention the r key for reverse sort")
	}
}

func TestFleetFooter_ReportsClusterCount(t *testing.T) {
	m := newFleetModel(200, 50)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "clusters") {
		t.Error("footer should report the cluster count")
	}
}

func TestFleetFooter_MentionsFilterKey(t *testing.T) {
	m := newFleetModel(200, 50)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "/ filter") {
		t.Error("footer should mention the / key for filtering")
	}
}

func TestFleetFilter_EntersMode(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.filtering {
		t.Error("/ should enter filter mode")
	}
}

func TestFleetFilter_NarrowsList(t *testing.T) {
	m := newFleetModel(200, 50)
	total := len(m.clusters)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant-01")})

	if m.filter != "tenant-01" {
		t.Errorf("filter should be %q, got %q", "tenant-01", m.filter)
	}
	filtered := m.filteredClusters()
	if len(filtered) >= total {
		t.Errorf("filter should narrow from %d, got %d", total, len(filtered))
	}
	for _, c := range filtered {
		if !strings.Contains(c.Name, "tenant-01") && !strings.Contains(c.Namespace, "tenant-01") {
			t.Errorf("cluster %q does not match filter", c.Name)
		}
	}
}

func TestFleetFilter_CaseInsensitive(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("TENANT")})
	if len(m.filteredClusters()) == 0 {
		t.Error("filter should be case-insensitive")
	}
}

func TestFleetFilter_EnterExitsModeKeepsFilter(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if m.filtering {
		t.Error("enter should exit filter mode")
	}
	if m.filter == "" {
		t.Error("filter should be kept after enter")
	}
}

func TestFleetFilter_EscClearsFilter(t *testing.T) {
	m := newFleetModel(200, 50)
	total := len(m.clusters)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant")})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.filtering {
		t.Error("esc should exit filter mode")
	}
	if m.filter != "" {
		t.Error("esc should clear the filter")
	}
	if len(m.filteredClusters()) != total {
		t.Errorf("after esc, should see all %d clusters, got %d", total, len(m.filteredClusters()))
	}
}

func TestFleetFilter_BackspaceEdits(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant-0")})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if m.filter != "tenant-" {
		t.Errorf("backspace should remove last char, got %q", m.filter)
	}
}

func TestFleetFilter_BackspaceEmptiesExitsMode(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if m.filtering {
		t.Error("backspace on empty filter should exit filter mode")
	}
	if m.filter != "" {
		t.Error("filter should be empty after backspace on one char")
	}
}

func TestFleetFilter_NoMatch(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz-no-match")})

	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "no clusters match") {
		t.Errorf("should show a no-match message:\n%s", firstLines(view, 10))
	}
}

func TestFleetFilter_FooterShowsFilteredCount(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant")})

	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "/") || !strings.Contains(view, "clusters") {
		t.Errorf("footer should show filtered count:\n%s", firstLines(view, 50))
	}
}

func TestFleetFilter_EnterDrillsFromFiltered(t *testing.T) {
	m := newFleetModel(200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant-01")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should drill into the filtered cluster")
	}
	msg := cmd()
	if _, ok := msg.(clusterDetailMsg); !ok {
		t.Errorf("enter should produce clusterDetailMsg, got %T", msg)
	}
}

func TestFleetFilter_SelectedClampedAfterNarrowing(t *testing.T) {
	m := newFleetModel(200, 50)
	for range len(m.clusters) {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tenant-01")})

	if m.selected >= len(m.filteredClusters()) {
		t.Errorf("selected %d should be clamped below %d", m.selected, len(m.filteredClusters()))
	}
}
