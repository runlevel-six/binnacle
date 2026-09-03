package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/testansi"
)

// failingSource is a fleet source that cannot be read.
type failingSource struct {
	*fleet.Demo
	problem  string
	clusters []fleet.ClusterView
}

func (f *failingSource) View() []fleet.ClusterView { return f.clusters }
func (f *failingSource) Problem() string           { return f.problem }

func renderFleet(t *testing.T, src fleetSource) string {
	t.Helper()
	m := NewFleet(src, "https://binnacle.example", build.Info{Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.Update(FleetUpdateMsg{
		Clusters: src.View(),
		Storage:  src.Storage(),
		Problem:  src.Problem(),
	})
	return testansi.StripANSI(m.View())
}

// An unreachable server must not render as an empty fleet. This is the rule
// the whole codebase turns on — NodesKnown, WorkloadProblem, the wire format's
// error fields — applied to the one screen that had no way to express it.
func TestFleet_UnreachableServerSaysSoRatherThanWaiting(t *testing.T) {
	view := renderFleet(t, &failingSource{
		Demo:    fleet.NewDemo(),
		problem: "cannot reach https://binnacle.example: connection refused",
	})

	if strings.Contains(view, "waiting for data") {
		t.Error("an unreachable server rendered as though data were still coming")
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("the reason is not on screen:\n%s", view)
	}
}

// An expired token is the failure most likely to happen mid-session, and the
// one with an obvious remedy — so the screen has to name it.
func TestFleet_ExpiredTokenNamesTheRemedy(t *testing.T) {
	view := renderFleet(t, &failingSource{
		Demo:    fleet.NewDemo(),
		problem: "not signed in to https://binnacle.example — the token may have expired; restart sextant to sign in again",
	})

	if !strings.Contains(view, "sign in again") {
		t.Errorf("expired token did not tell the operator what to do:\n%s", view)
	}
}

// Data plus a problem is the stale case: the numbers are the last good read,
// which is worth showing but not worth showing silently.
func TestFleet_StaleDataIsLabeled(t *testing.T) {
	demo := fleet.NewDemo()
	view := renderFleet(t, &failingSource{
		Demo:     demo,
		problem:  "cannot reach https://binnacle.example: timeout",
		clusters: demo.View(),
	})

	if !strings.Contains(view, "stale") {
		t.Errorf("stale data was shown as current:\n%s", view)
	}
	if !strings.Contains(view, "CLUSTER") {
		t.Error("the last good read should still be rendered")
	}
}

// A healthy fleet gains no warning furniture.
func TestFleet_NoProblemNoNoise(t *testing.T) {
	demo := fleet.NewDemo()
	view := renderFleet(t, demo)

	for _, unwanted := range []string{"stale", "cannot reach", "sign in"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("healthy fleet showed %q:\n%s", unwanted, view)
		}
	}
}
