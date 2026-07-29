package metallb

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// pane renders MetalLB's pools and their advertisement state.
type pane struct {
	store *store.Store
}

func newPane(s *store.Store) *pane { return &pane{store: s} }

func (p *pane) ID() string             { return "metallb" }
func (p *pane) Title() string          { return "MetalLB" }
func (p *pane) Priority() tui.Priority { return tui.P1Important }
func (p *pane) MinWidth() int          { return 44 }
func (p *pane) MinHeight() int         { return 6 }
func (p *pane) HeightWeight() int      { return 2 }

// Group puts this pane in the shared "Network" frame; see [tui.GroupedPane].
func (p *pane) GroupID() string    { return "network" }
func (p *pane) GroupTitle() string { return "Network" }
func (p *pane) GroupOrder() int    { return 1 }

var poolCols = []table.Column{
	{Header: "POOL"},
	{Header: "ADDRESSES", Stretch: true},
	{Header: "ADVERTISED"},
	{Header: "IN USE"},
}

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading MetalLB…")
	}
	if state.Err != nil && len(state.Pools) == 0 {
		return table.ErrorBody(w, h, state.Err)
	}
	if len(state.Pools) == 0 {
		return table.Placeholder(w, h, "no address pools")
	}

	cells := make([][]string, 0, len(state.Pools))
	styles := make([][]lipgloss.Style, 0, len(state.Pools))
	for _, pool := range state.Pools {
		advertised, advStyle := "none", tui.StyleErr
		if len(pool.Advertised) > 0 {
			advertised, advStyle = strings.Join(pool.Advertised, "+"), tui.StyleOK
		}
		name := pool.Name
		if !pool.AutoAssign {
			// A manual-only pool will never satisfy a Service that does not ask
			// for it by name, which explains an otherwise baffling Pending.
			name += " (manual)"
		}
		cells = append(cells, []string{
			name,
			strings.Join(pool.Addresses, ", "),
			advertised,
			fmt.Sprintf("%d", pool.Assigned),
		})
		styles = append(styles, []lipgloss.Style{{}, tui.StyleMuted, advStyle, {}})
	}

	body := table.Table{Cols: poolCols, Rows: cells, CellStyles: styles}
	summary := p.summary(state)
	if summary == "" || h < len(cells)+3 {
		return body.Render(w, h)
	}
	return clip(body.Render(w, h-2)+"\n\n"+summary, w, h)
}

// summary reports the speaker and any pending Services — the two things that
// explain why a LoadBalancer is not working.
func (p *pane) summary(state State) string {
	speaker := tui.StyleOK.Render(fmt.Sprintf("speaker %d/%d", state.SpeakerReady, state.SpeakerDesired))
	if state.SpeakerDesired == 0 {
		speaker = tui.StyleMuted.Render("speaker not found")
	} else if state.SpeakerReady < state.SpeakerDesired {
		speaker = tui.StyleErr.Render(fmt.Sprintf("speaker %d/%d", state.SpeakerReady, state.SpeakerDesired))
	}

	parts := []string{speaker}
	if n := state.PendingServices(); n > 0 {
		parts = append(parts, tui.StyleWarn.Render(fmt.Sprintf("%d Service(s) pending an address", n)))
	} else if len(state.Services) > 0 {
		parts = append(parts, tui.StyleMuted.Render(fmt.Sprintf("%d LoadBalancer Service(s)", len(state.Services))))
	}
	return strings.Join(parts, "  ")
}

// clip bounds a composed body, since this pane appends a summary after a table.
func clip(body string, w, h int) string {
	lines := strings.Split(body, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, ln := range lines {
		if lipgloss.Width(ln) > w {
			lines[i] = table.PadOrTrunc(ln, w)
		}
	}
	return strings.Join(lines, "\n")
}
