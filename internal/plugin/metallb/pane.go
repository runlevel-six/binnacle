package metallb

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
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

// ContentWidth implements [tui.ContentWidthPane]: the pool table.
//
// Not the speaker summary under it. That line grows a clause whenever a Service is
// waiting for an address, and a tile that resized when it did would move every
// boundary in its row for a sentence that truncates harmlessly.
func (p *pane) ContentWidth() int {
	cells, _ := p.content()
	if len(cells) == 0 {
		return 0
	}
	return table.AppetiteWidth(poolCols, cells)
}

// ContentHeight implements [tui.ContentHeightPane]: a header, one row per address
// pool, and the summary line with its blank separator. A cluster's pools are
// configuration rather than fleet, so this is a number that changes when someone
// changes it and not otherwise.
func (p *pane) ContentHeight(int) int {
	cells, summary := p.content()
	if len(cells) == 0 {
		return 0
	}
	h := len(cells) + 1
	if summary != "" {
		h += 2
	}
	return h
}

// content builds the pool rows and the summary line, shared by the extent methods
// so the pane cannot declare one shape and draw another. The cell styles are left
// to Render, which is the only caller that paints anything.
func (p *pane) content() (cells [][]string, summary string) {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || len(state.Pools) == 0 {
		return nil, ""
	}
	cells, _ = p.poolRows(state)
	return cells, p.summary(state)
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

	cells, styles := p.poolRows(state)
	body := table.Table{Cols: poolCols, Rows: cells, CellStyles: styles}
	summary := p.summary(state)
	if summary == "" || h < len(cells)+3 {
		return body.Render(w, h)
	}
	return clip(body.Render(w, h-2)+"\n\n"+summary, w, h)
}

// poolRows renders one row per address pool.
func (p *pane) poolRows(state State) (cells [][]string, styles [][]lipgloss.Style) {
	cells = make([][]string, 0, len(state.Pools))
	styles = make([][]lipgloss.Style, 0, len(state.Pools))
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
		used, usedStyle := usage(pool)
		cells = append(cells, []string{
			name,
			strings.Join(pool.Addresses, ", "),
			advertised,
			used,
		})
		styles = append(styles, []lipgloss.Style{{}, tui.StyleMuted, advStyle, usedStyle})
	}
	return cells, styles
}

// nearlyFull is the fraction of a pool left at which its remaining addresses are
// worth a color.
//
// A tenth is late enough not to cry wolf on a pool that is merely being used,
// and early enough to be a warning rather than a postmortem: a /24 flags with
// twenty-five addresses to go, which is a few days of ordinary churn.
const nearlyFull = 0.1

// usage renders the IN USE cell.
//
// Where MetalLB publishes a pool's counts the cell is used-of-total, because the
// number an operator wants is not how many addresses are out but how many are
// left — this pane's stated reason for existing. Where only Services could be
// attributed there is no honest total, so the bare count stands alone. Where
// nothing could attribute them the cell says so: a zero would read as an idle
// pool, and an idle pool is a pool somebody deletes.
func usage(pool Pool) (string, lipgloss.Style) {
	switch pool.Usage {
	case UsageUnknown:
		return "?", tui.StyleMuted
	case UsageAnnotations:
		return fmt.Sprintf("%d", pool.Assigned), lipgloss.Style{}
	}

	cell := fmt.Sprintf("%d/%d", pool.Assigned, pool.Total())
	switch {
	case pool.Exhausted():
		return cell, tui.StyleErr
	case pool.Total() > 0 && float64(pool.Available)/float64(pool.Total()) <= nearlyFull:
		return cell, tui.StyleWarn
	}
	return cell, lipgloss.Style{}
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
	// Version drift, only while there is any: a speaker fleet mid-roll is worth a
	// word, a converged one is not.
	if r := state.Rollout; r.Known() && !r.Converged() {
		parts = append(parts, tui.StyleAccent.Render(
			fmt.Sprintf("%d/%d updated", r.Updated, r.Desired)))
	}
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
