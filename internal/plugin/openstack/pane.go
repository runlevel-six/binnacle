package openstack

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

// pane renders the cloud's control-plane health.
type pane struct {
	store *store.Store
}

func newPane(s *store.Store) *pane { return &pane{store: s} }

func (p *pane) ID() string             { return "openstack" }
func (p *pane) Title() string          { return "OpenStack" }
func (p *pane) Priority() tui.Priority { return tui.P1Important }
func (p *pane) MinWidth() int          { return 46 }
func (p *pane) MinHeight() int         { return 6 }
func (p *pane) HeightWeight() int      { return 3 }

// Group puts this pane in the shared "Cloud" frame; see [tui.GroupedPane].
func (p *pane) GroupID() string    { return "cloud" }
func (p *pane) GroupTitle() string { return "Cloud" }
func (p *pane) GroupOrder() int    { return 0 }

var serviceCols = []table.Column{
	{Header: "SERVICE"},
	{Header: "AGENTS"},
	{Header: "DISABLED"},
	{Header: "NOTE", Stretch: true, Transient: true},
}

// ContentWidth implements [tui.ContentWidthPane]: the service table.
//
// Not the agent detail beneath it. That line names the down and disabled binaries,
// so it is the most volatile string in the pane, and pinning a tile width to it
// would move the row every time an agent recovered.
func (p *pane) ContentWidth() int {
	cells, _, _ := p.content()
	if len(cells) == 0 {
		return 0
	}
	return table.AppetiteWidth(serviceCols, cells)
}

// ContentHeight implements [tui.ContentHeightPane]: a header, one row per
// OpenStack service, and the agent detail with its separator. The service list is
// what the cloud has deployed, not what it is running.
func (p *pane) ContentHeight(int) int {
	cells, _, detail := p.content()
	if len(cells) == 0 {
		return 0
	}
	h := len(cells) + 1
	if detail != "" {
		h += 2
	}
	return h
}

// content builds the service rows and the agent detail, shared by Render and the
// extent methods.
func (p *pane) content() (cells [][]string, styles [][]lipgloss.Style, detail string) {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || state.Err != nil || len(state.Services) == 0 {
		return nil, nil, ""
	}
	cells = make([][]string, 0, len(state.Services))
	styles = make([][]lipgloss.Style, 0, len(state.Services))
	for _, s := range state.Services {
		cells = append(cells, serviceRow(s))
		styles = append(styles, serviceStyles(s))
	}
	return cells, styles, agentDetail(state)
}

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading OpenStack…")
	}
	if state.Err != nil {
		return table.ErrorBody(w, h, state.Err)
	}
	if len(state.Services) == 0 {
		return table.Placeholder(w, h, "no services reported")
	}

	cells, styles, detail := p.content()
	body := table.Table{Cols: serviceCols, Rows: cells, CellStyles: styles}
	if detail == "" || h < len(cells)+3 {
		return body.Render(w, h)
	}
	return clip(body.Render(w, h-2)+"\n\n"+detail, w, h)
}

func serviceRow(s ServiceSummary) []string {
	if s.Err != nil {
		return []string{s.Service, "—", "—", "unavailable: " + s.Err.Error()}
	}

	disabled := "—"
	if s.Disabled > 0 {
		disabled = fmt.Sprintf("%d", s.Disabled)
	}
	note := ""
	if len(s.DownBinaries) > 0 {
		// Name the binaries rather than only counting them: "3 down" does not say
		// whether it is every compute node or one scheduler.
		note = "down: " + strings.Join(s.DownBinaries, ", ")
	}
	return []string{s.Service, fmt.Sprintf("%d/%d up", s.Up, s.Total), disabled, note}
}

func serviceStyles(s ServiceSummary) []lipgloss.Style {
	if s.Err != nil {
		return []lipgloss.Style{{}, {}, {}, tui.StyleWarn}
	}

	agentStyle := tui.StyleOK
	switch {
	case len(s.DownBinaries) > 0:
		agentStyle = tui.StyleErr
	case s.Disabled > 0 && s.Up < s.Total:
		// Not everything is up, but only because something is disabled — which is
		// deliberate, so this is not an error.
		agentStyle = tui.StyleWarn
	}

	disabledStyle := tui.StyleMuted
	if s.Disabled > 0 {
		disabledStyle = tui.StyleWarn
	}
	return []lipgloss.Style{{}, agentStyle, disabledStyle, tui.StyleErr}
}

// agentDetail names the specific agents needing attention.
//
// Down agents come first and are named by host, since that is the thing to go and
// look at. Disabled ones follow, because a node left disabled after maintenance is
// a common oversight and nothing else on screen would reveal it.
func agentDetail(state State) string {
	var lines []string
	if down := state.DownAgents(); len(down) > 0 {
		lines = append(lines, tui.StyleErr.Render("Down: ")+strings.Join(hostList(down), ", "))
	}
	if disabled := state.DisabledAgents(); len(disabled) > 0 {
		lines = append(lines, tui.StyleWarn.Render("Disabled: ")+strings.Join(hostList(disabled), ", "))
	}
	return strings.Join(lines, "\n")
}

// hostList formats agents as "binary@host".
func hostList(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, fmt.Sprintf("%s@%s", a.Binary, ShortHost(a.Host)))
	}
	return out
}

// ShortHost trims a host to its first DNS label.
//
// Compute hosts are FQDNs — "compute-node-1.site-a.example.com" — and the domain
// is identical on every row, so it is pure noise in a table.
func ShortHost(host string) string {
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
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
