package ovn

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// rolloutPane reports whether the switching layer is running the version its
// charts ask for.
//
// A section of its own rather than a line under the Raft table, because the two
// answer different questions on different clocks. Raft health is about this
// second and is usually fine; a rollout is about a change someone started and may
// not have finished, and on the Open vSwitch path it can sit half-done for weeks
// with nothing else on the dashboard saying so.
type rolloutPane struct {
	store *store.Store
}

func newRolloutPane(s *store.Store) *rolloutPane { return &rolloutPane{store: s} }

func (p *rolloutPane) ID() string             { return "ovn-rollout" }
func (p *rolloutPane) Title() string          { return "OVN / OVS Version" }
func (p *rolloutPane) Priority() tui.Priority { return tui.P2Useful }
func (p *rolloutPane) MinWidth() int          { return 44 }
func (p *rolloutPane) MinHeight() int         { return 4 }
func (p *rolloutPane) HeightWeight() int      { return 2 }

// Group puts this pane in the shared "Network" frame; see [tui.GroupedPane].
func (p *rolloutPane) GroupID() string    { return "network" }
func (p *rolloutPane) GroupTitle() string { return "Network" }
func (p *rolloutPane) GroupOrder() int    { return 3 }

var rolloutCols = []table.Column{
	{Header: "COMPONENT"},
	{Header: "UP TO DATE"},
	{Header: "PENDING", Stretch: true},
}

// Render implements tui.Pane.
func (p *rolloutPane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "polling OVN workloads…")
	}
	if len(state.Components) == 0 {
		return table.Placeholder(w, h, "no OVN workloads found")
	}

	// Too short for a table; see the same rule in the OpenStack services pane. A
	// header over "+ 4 more" is two lines that name nothing.
	if h < 3 {
		line := rolloutSummary(state.Components)
		if line == "" {
			line = tui.StyleOK.Render(fmt.Sprintf("%d component(s) up to date", len(state.Components)))
		}
		return table.ClipLines(table.PadOrTrunc(line, w), h)
	}

	// The converged components are the first thing to give up when the frame is
	// short, and giving them up costs nothing: "ovsdb-nb 3/3" is a row that says
	// the same thing every day for months. Dropping them in favour of the ones
	// that are behind is the difference between a table that truncates to
	// "+ 4 more" — hiding the only rows worth reading — and one that shows the
	// work outstanding and counts the rest on a single line.
	shown := state.Components
	folded := 0
	if len(shown)+2 > h {
		if pending := PendingComponents(state.Components); len(pending) > 0 {
			folded = len(state.Components) - len(pending)
			shown = pending
		}
	}

	rows := make([][]string, 0, len(shown))
	styles := make([][]lipgloss.Style, 0, len(shown))
	for _, c := range shown {
		rows = append(rows, componentRow(c))
		styles = append(styles, componentStyles(c))
	}

	lines := []string{table.Table{Cols: rolloutCols, Rows: rows, CellStyles: styles}.Render(w, min(h, len(rows)+1))}
	if folded > 0 {
		lines = append(lines, table.PadOrTrunc(
			tui.StyleMuted.Render(fmt.Sprintf("%d other component(s) up to date", folded)), w))
	}
	if summary := rolloutSummary(state.Components); summary != "" {
		lines = append(lines, table.PadOrTrunc(summary, w))
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
}

func componentRow(c Component) []string {
	count := fmt.Sprintf("%d/%d", c.Updated, c.Desired)
	if c.Converged() {
		return []string{c.Name, count, ""}
	}

	if c.Manual {
		// The distinction the whole pane exists for, and it has to be legible at a
		// glance rather than spelled out per row: a prose prefix on every line ate
		// the width the node names needed, and the summary underneath already says
		// what the marker means.
		count += " ⚠"
	}

	note := ""
	if len(c.StaleNodes) > 0 {
		// Named rather than counted, because finishing a manual rollout is a
		// per-host job and the next host is the only thing the operator needs.
		// Two fit beside the count in one grid column; the rest are already in
		// the fraction to the left.
		shown, more := kube.ShortNodeNames(c.StaleNodes, 2)
		note = strings.Join(shown, ", ")
		if more > 0 {
			note += fmt.Sprintf(", +%d", more)
		}
	} else if c.Manual {
		note = "awaiting drain"
	}
	return []string{c.Name, count, note}
}

func componentStyles(c Component) []lipgloss.Style {
	switch {
	case c.Converged():
		return []lipgloss.Style{{}, tui.StyleOK, {}}
	case c.Manual:
		// Amber, not red. Nothing is broken — an operator has work to do, which
		// is a different thing and must not read as an outage.
		return []lipgloss.Style{{}, tui.StyleWarn, tui.StyleWarn}
	default:
		return []lipgloss.Style{{}, tui.StyleAccent, tui.StyleMuted}
	}
}

// rolloutSummary is the one-line headline, or empty when everything is current.
//
// Silent when converged, on the same rule the overview blocks follow: a line that
// is always present is one nobody reads, and this pane's whole value is being
// noticed on the day it has something to say.
func rolloutSummary(cs []Component) string {
	pending := PendingComponents(cs)
	if len(pending) == 0 {
		return ""
	}

	var stale int32
	manual := false
	for _, c := range pending {
		stale += c.Stale()
		manual = manual || c.Manual
	}
	msg := fmt.Sprintf("%d pod(s) on a superseded version", stale)
	if manual {
		return tui.StyleWarn.Render(msg + " — will not roll on its own")
	}
	return tui.StyleAccent.Render(msg)
}
