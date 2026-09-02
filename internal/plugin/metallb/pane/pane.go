package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/pkg/store"
	metallbstate "github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

const (
	KeyState          = metallbstate.KeyState
	UsageUnknown      = metallbstate.UsageUnknown
	UsageAnnotations  = metallbstate.UsageAnnotations
	UsageStatus       = metallbstate.UsageStatus
)

type (
	Pool       = metallbstate.Pool
	UsageSource = metallbstate.UsageSource
	Service    = metallbstate.Service
	State      = metallbstate.State
)

type Provider struct{}

func NewProvider() *Provider { return &Provider{} }

func (p *Provider) Name() string { return "metallb" }

func (p *Provider) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{newPane(s)}
}

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
func (p *pane) ContentWidth() int {
	cells, _ := p.content()
	if len(cells) == 0 {
		return 0
	}
	return table.AppetiteWidth(poolCols, cells)
}

// ContentHeight implements [tui.ContentHeightPane]: a header, one row per address
// pool, and the summary line with its blank separator.
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

const nearlyFull = 0.1

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

func (p *pane) summary(state State) string {
	speaker := tui.StyleOK.Render(fmt.Sprintf("speaker %d/%d", state.SpeakerReady, state.SpeakerDesired))
	if state.SpeakerDesired == 0 {
		speaker = tui.StyleMuted.Render("speaker not found")
	} else if state.SpeakerReady < state.SpeakerDesired {
		speaker = tui.StyleErr.Render(fmt.Sprintf("speaker %d/%d", state.SpeakerReady, state.SpeakerDesired))
	}

	parts := []string{speaker}
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
