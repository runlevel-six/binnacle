package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// fleetSource is the subset of web.Source the fleet model needs. Defined
// locally so the UI package does not import the web package — both the live
// server and the remote client satisfy it.
type fleetSource interface {
	View() []fleet.ClusterView
	Cluster(namespace, name string) (fleet.ClusterDetail, bool)
	Storage() fleet.Storage
	Changed() <-chan struct{}
}

// FleetModel is the Bubble Tea model for --server mode: a fleet list with
// drill-down to cluster detail.
type FleetModel struct {
	source   fleetSource
	serverURL string
	buildInfo build.Info

	clusters []fleet.ClusterView
	storage  fleet.Storage

	selected int
	detail   *fleet.ClusterDetail
	viewing  string // "fleet" or "detail"

	width, height int
	keys          fleetKeymap
	err           string
}

// NewFleet builds a fleet model reading from src.
func NewFleet(src fleetSource, serverURL string, info build.Info) *FleetModel {
	return &FleetModel{
		source:    src,
		serverURL: serverURL,
		buildInfo: info,
		viewing:   "fleet",
		keys:      defaultFleetKeymap(),
	}
}

// fleetUpdateMsg carries a fresh fleet view.
type fleetUpdateMsg struct {
	clusters []fleet.ClusterView
	storage  fleet.Storage
}

// changedMsg signals that the source reported a change.
type changedMsg struct{}

// clusterDetailMsg carries one cluster's detail.
type clusterDetailMsg struct {
	detail fleet.ClusterDetail
	ok     bool
}

// fetchFleet reads the current fleet view from the source.
func fetchFleet(src fleetSource) tea.Cmd {
	return func() tea.Msg {
		return fleetUpdateMsg{
			clusters: src.View(),
			storage:  src.Storage(),
		}
	}
}

// waitForChanged blocks until the source reports a change.
func waitForChanged(src fleetSource) tea.Cmd {
	return func() tea.Msg {
		<-src.Changed()
		return changedMsg{}
	}
}

// fetchCluster reads one cluster's detail from the source.
func fetchCluster(src fleetSource, ns, name string) tea.Cmd {
	return func() tea.Msg {
		d, ok := src.Cluster(ns, name)
		return clusterDetailMsg{detail: d, ok: ok}
	}
}

// Init implements tea.Model.
func (m *FleetModel) Init() tea.Cmd {
	return tea.Batch(fetchFleet(m.source), waitForChanged(m.source))
}

// Update implements tea.Model.
func (m *FleetModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case fleetUpdateMsg:
		m.clusters = msg.clusters
		m.storage = msg.storage
		if m.selected >= len(m.clusters) {
			m.selected = max(len(m.clusters)-1, 0)
		}
		return m, nil

	case changedMsg:
		return m, tea.Batch(fetchFleet(m.source), waitForChanged(m.source))

	case clusterDetailMsg:
		if msg.ok {
			m.detail = &msg.detail
			m.viewing = "detail"
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleFleetKey(msg)
	}
	return m, nil
}

func (m *FleetModel) handleFleetKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.viewing == "detail" {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Back):
			m.viewing = "fleet"
			m.detail = nil
			return m, nil
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Up):
		if m.selected > 0 {
			m.selected--
		}
	case key.Matches(msg, m.keys.Down):
		if m.selected < len(m.clusters)-1 {
			m.selected++
		}
	case key.Matches(msg, m.keys.Enter):
		if len(m.clusters) > 0 {
			c := m.clusters[m.selected]
			return m, fetchCluster(m.source, c.Namespace, c.Name)
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m *FleetModel) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	if m.viewing == "detail" && m.detail != nil {
		return m.renderDetail()
	}
	return m.renderFleet()
}

func (m *FleetModel) renderFleet() string {
	th := tui.CurrentTheme()

	header := m.fleetHeader()
	body := m.fleetBody(th)
	footer := m.fleetFooter(th)

	lines := []string{header, body}
	if h := m.height - lipgloss.Height(header) - lipgloss.Height(footer); h > 0 {
		lines = append(lines, strings.Repeat("\n", h-1))
	}
	lines = append(lines, footer)

	result := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return padToWidth(result, m.width)
}

func (m *FleetModel) fleetHeader() string {
	th := tui.CurrentTheme()
	hs := newHeaderStyles()

	title := hs.title.Render(th.Label("sextant"))
	if v := m.buildInfo.Short(); v != "" {
		title += hs.dim.Render(" " + v)
	}
	server := hs.bar.Render(th.Label("server")+" ") + hs.title.Render(m.serverURL)

	content := truncate(title+hs.dim.Render(th.Separator)+server, m.width-2)
	return barRow(hs, content, m.width)
}

func (m *FleetModel) fleetBody(th tui.Theme) string {
	if len(m.clusters) == 0 {
		return tui.StyleMuted.Render("waiting for data…")
	}

	header := fmt.Sprintf("%-3s  %-30s  %-10s  %-8s  %s",
		"", "CLUSTER", "VERSION", "NODES", "PROBLEM")
	body := tui.StyleMuted.Render(header) + "\n"

	availH := m.height - 4 // header(2) + footer(1) + column header(1)
	if availH < 1 {
		availH = 1
	}

	end := min(m.selected+availH, len(m.clusters))
	start := max(0, end-availH)

	for i := start; i < end; i++ {
		c := m.clusters[i]
		marker := " "
		if i == m.selected {
			marker = ">"
		}

		statusGlyph := statusGlyph(c.Status, th)
		name := truncateStr(c.Name, 30)
		version := truncateStr(c.Version, 10)
		nodes := fmt.Sprintf("%d/%d", c.Nodes.Ready, c.Nodes.Total)
		if !c.NodesKnown {
			nodes = "?"
		}

		problem := c.Problem
		if problem == "" {
			problem = c.WorkloadProblem
		}
		problem = truncateStr(problem, 40)

		style := lipgloss.NewStyle()
		if th.Grounded() {
			style = style.Background(th.PaneBG)
		}

		row := fmt.Sprintf("%s %-2s  %-30s  %-10s  %-8s  %s",
			marker, statusGlyph, name, version, nodes, problem)

		if i == m.selected {
			row = lipgloss.NewStyle().Bold(true).Render(row)
		}

		body += row + "\n"
	}

	return strings.TrimRight(body, "\n")
}

func (m *FleetModel) fleetFooter(th tui.Theme) string {
	status := styleStatusBar()
	count := fmt.Sprintf("%d clusters", len(m.clusters))
	if len(m.storage.Clusters) > 0 {
		count += fmt.Sprintf(" · %d storage clusters", len(m.storage.Clusters))
	}
	hint := "? help · ↑↓ navigate · enter detail · q quit"
	return groundLine(
		status.Render(count)+status.Render(th.Separator)+status.Render(hint),
		m.width, th.Text, th.ScreenBG(),
	)
}

func (m *FleetModel) renderDetail() string {
	th := tui.CurrentTheme()
	d := m.detail

	var sb strings.Builder

	// Title line
	title := tui.StyleHeader.Bold(true).Render(d.Name)
	if d.Namespace != "" {
		title += tui.StyleMuted.Render(" (" + d.Namespace + ")")
	}
	sb.WriteString(title + "\n\n")

	// Status and version
	sb.WriteString(fmt.Sprintf("Status:   %s\n", statusText(d.Status, th)))
	if d.Version != "" {
		sb.WriteString(fmt.Sprintf("Version:  %s\n", d.Version))
	}
	if d.Phase != "" {
		sb.WriteString(fmt.Sprintf("Phase:    %s\n", d.Phase))
	}
	sb.WriteString("\n")

	// Health cells
	if len(d.Cells) > 0 {
		sb.WriteString("Health:\n")
		for _, c := range d.Cells {
			glyph := statusGlyph(c.Status, th)
			line := fmt.Sprintf("  %s %s", glyph, c.Name)
			if c.Detail != "" {
				line += tui.StyleMuted.Render(" — " + c.Detail)
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}

	// Node pools
	if len(d.Pools) > 0 {
		sb.WriteString("Node Pools:\n")
		for _, p := range d.Pools {
			sb.WriteString(fmt.Sprintf("  %-30s  %s  %d/%d  %s\n",
				p.Name, p.Role, p.Ready, p.Desired, p.Version))
		}
		sb.WriteString("\n")
	}

	// Problems
	if d.Problem != "" {
		sb.WriteString(tui.StyleErr.Render("Problem: "+d.Problem) + "\n")
	}
	if d.WorkloadProblem != "" {
		sb.WriteString(tui.StyleWarn.Render("Workload: "+d.WorkloadProblem) + "\n")
	}
	if d.CloudsProblem != "" {
		sb.WriteString(tui.StyleWarn.Render("Clouds: "+d.CloudsProblem) + "\n")
	}

	// Machines summary
	if d.Machines.Total() > 0 {
		sb.WriteString(fmt.Sprintf("\nMachines: %d shown, %d total\n",
			len(d.Machines.Shown), d.Machines.Total()))
	}

	// Hosts summary
	if d.Hosts.Total() > 0 {
		sb.WriteString(fmt.Sprintf("Hosts:    %d shown, %d total", len(d.Hosts.Shown), d.Hosts.Total()))
		if d.HostsElsewhere > 0 {
			sb.WriteString(fmt.Sprintf(" (%d elsewhere)", d.HostsElsewhere))
		}
		sb.WriteString("\n")
	}

	// Nodes summary
	if d.NodeRows.Total() > 0 {
		sb.WriteString(fmt.Sprintf("Nodes:    %d shown, %d total\n",
			len(d.NodeRows.Shown), d.NodeRows.Total()))
	}

	// Events summary
	if d.EventsTotal > 0 {
		sb.WriteString(fmt.Sprintf("Events:   %d groups, %d total", len(d.Events.Shown), d.EventsTotal))
		if d.EventsTruncated > 0 {
			sb.WriteString(fmt.Sprintf(" (%d truncated)", d.EventsTruncated))
		}
		sb.WriteString("\n")
	}

	// Updated
	if !d.UpdatedAt.IsZero() {
		age := time.Since(d.UpdatedAt).Round(time.Second)
		sb.WriteString(fmt.Sprintf("\nUpdated %s ago\n", age))
	}

	// Footer
	footer := "\n" + tui.StyleMuted.Render("esc back · q quit")
	sb.WriteString(footer)

	return padToWidth(sb.String(), m.width)
}

func statusGlyph(s health.Status, th tui.Theme) string {
	switch s {
	case health.StatusOK:
		return tui.StyleOK.Render(th.GlyphOK)
	case health.StatusWarn:
		return tui.StyleWarn.Render(th.GlyphWarn)
	case health.StatusErr:
		return tui.StyleErr.Render(th.GlyphErr)
	}
	return tui.StyleMuted.Render(th.GlyphLoading)
}

func statusText(s health.Status, th tui.Theme) string {
	style := tui.StyleMuted
	switch s {
	case health.StatusOK:
		style = tui.StyleOK
	case health.StatusWarn:
		style = tui.StyleWarn
	case health.StatusErr:
		style = tui.StyleErr
	}
	return style.Render(s.String())
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// fleetKeymap is the key set for the fleet model.
type fleetKeymap struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Back  key.Binding
	Quit  key.Binding
	Help  key.Binding
}

func defaultFleetKeymap() fleetKeymap {
	return fleetKeymap{
		Up:    key.NewBinding(key.WithKeys("up", "k")),
		Down:  key.NewBinding(key.WithKeys("down", "j")),
		Enter: key.NewBinding(key.WithKeys("enter")),
		Back:  key.NewBinding(key.WithKeys("esc", "backspace")),
		Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c")),
		Help:  key.NewBinding(key.WithKeys("?")),
	}
}
