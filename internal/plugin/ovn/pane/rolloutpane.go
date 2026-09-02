package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

// rolloutPane reports whether the switching layer is running the version its
// charts ask for.
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

func (p *rolloutPane) GroupID() string    { return "network" }
func (p *rolloutPane) GroupTitle() string { return "Network" }
func (p *rolloutPane) GroupOrder() int    { return 3 }

var rolloutCols = []table.Column{
	{Header: "COMPONENT"},
	{Header: "UP TO DATE"},
	{Header: "PENDING", Stretch: true, Transient: true},
}

func (p *rolloutPane) ContentWidth() int {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || len(state.Components) == 0 {
		return 0
	}
	rows, _ := p.componentRows(state.Components)
	return table.AppetiteWidth(rolloutCols, rows)
}

func (p *rolloutPane) ContentHeight(int) int {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || len(state.Components) == 0 {
		return 0
	}
	h := len(state.Components) + 1
	if rolloutSummary(state.Components) != "" {
		h++
	}
	return h
}

func (p *rolloutPane) componentRows(cs []Component) (rows [][]string, styles [][]lipgloss.Style) {
	rows = make([][]string, 0, len(cs))
	styles = make([][]lipgloss.Style, 0, len(cs))
	for _, c := range cs {
		rows = append(rows, componentRow(c))
		styles = append(styles, componentStyles(c))
	}
	return rows, styles
}

func (p *rolloutPane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "polling OVN workloads…")
	}
	if len(state.Components) == 0 {
		return table.Placeholder(w, h, "no OVN workloads found")
	}

	if h < 3 {
		line := rolloutSummary(state.Components)
		if line == "" {
			line = tui.StyleOK.Render(fmt.Sprintf("%d component(s) up to date", len(state.Components)))
		}
		return table.ClipLines(table.PadOrTrunc(line, w), h)
	}

	shown := state.Components
	folded := 0
	if len(shown)+2 > h {
		if pending := PendingComponents(state.Components); len(pending) > 0 {
			folded = len(state.Components) - len(pending)
			shown = pending
		}
	}

	rows, styles := p.componentRows(shown)
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
		count += " ⚠"
	}

	note := ""
	if len(c.StaleNodes) > 0 {
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
		return []lipgloss.Style{{}, tui.StyleWarn, tui.StyleWarn}
	default:
		return []lipgloss.Style{{}, tui.StyleAccent, tui.StyleMuted}
	}
}

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
