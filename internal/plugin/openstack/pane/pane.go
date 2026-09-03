// Package pane renders the OpenStack panes.
//
// Separate from the plugin beside it so the collector carries no dependency on
// a renderer — the same split every subsystem makes, and the reason a web
// server can link the data layer without linking a terminal library.
package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/plugin/openstack"
	"github.com/runlevel-six/binnacle/pkg/store"
	osstate "github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

const (
	KeyState            = osstate.KeyState
	KeyMigrations       = osstate.KeyMigrations
	KeyInventory        = osstate.KeyInventory
	ServiceCompute      = osstate.ServiceCompute
	ServiceNetwork      = osstate.ServiceNetwork
	ServiceBlockStorage = osstate.ServiceBlockStorage
)

type (
	State          = osstate.State
	ServiceSummary = osstate.ServiceSummary
	Agent          = osstate.Agent
	Migration      = osstate.Migration
	Migrations     = osstate.Migrations
	BrokenServer   = osstate.BrokenServer
	Drain          = osstate.Drain
	Inventory      = osstate.Inventory
	Count          = osstate.Count
	Shown          = osstate.Shown
	Service        = openstack.Service
	Services       = openstack.Services
	Component      = openstack.Component
)

var (
	ShortType     = osstate.ShortType
	ShortStatus   = osstate.ShortStatus
	DrainingHosts = osstate.DrainingHosts
)

// Provider contributes the OpenStack panes. It implements [tui.PaneProvider].
type Provider struct {
	namespace     string
	targetVersion string
}

func NewProvider(namespace, targetVersion string) *Provider {
	return &Provider{namespace: namespace, targetVersion: targetVersion}
}

func (p *Provider) Name() string { return "openstack" }

// Panes contributes the cloud's control-plane health, service versions, resource
// census, and server migrations.
func (p *Provider) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{
		newPane(s),
		newServicesPane(s, p.namespace),
		newResourcesPane(s),
		newCloudPane(s, p.targetVersion),
	}
}

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

func (p *pane) GroupID() string    { return "cloud" }
func (p *pane) GroupTitle() string { return "Cloud" }
func (p *pane) GroupOrder() int    { return 0 }

var serviceCols = []table.Column{
	{Header: "SERVICE"},
	{Header: "AGENTS"},
	{Header: "DISABLED"},
	{Header: "NOTE", Stretch: true, Transient: true},
}

func (p *pane) ContentWidth() int {
	cells, _, _ := p.content()
	if len(cells) == 0 {
		return 0
	}
	return table.AppetiteWidth(serviceCols, cells)
}

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
		agentStyle = tui.StyleWarn
	}

	disabledStyle := tui.StyleMuted
	if s.Disabled > 0 {
		disabledStyle = tui.StyleWarn
	}
	return []lipgloss.Style{{}, agentStyle, disabledStyle, tui.StyleErr}
}

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

func hostList(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, fmt.Sprintf("%s@%s", a.Binary, ShortHost(a.Host)))
	}
	return out
}

// ShortHost trims a host to its first DNS label.
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
