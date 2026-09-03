package ui

import (
	"fmt"
	"sort"
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
	// Problem reports why the fleet could not be read, or empty when it
	// could. Without it an unreachable server and an empty fleet render
	// identically, and a screen that cannot reach anything says nothing is
	// wrong.
	Problem() string
}

// FleetModel is the Bubble Tea model for --server mode: a fleet list with
// drill-down to cluster detail.
type FleetModel struct {
	source    fleetSource
	serverURL string
	buildInfo build.Info

	clusters []fleet.ClusterView
	storage  fleet.Storage
	reversed bool

	selected   int
	detail     *fleet.ClusterDetail
	viewing    string // "fleet" or "detail"
	autoSelect string // "namespace/name" to drill into on first load

	// filter: when non-empty, only clusters whose namespace/name contain
	// the substring are shown. filtering is a mode entered with / and
	// exited with enter or esc.
	filter    string
	filtering bool

	width, height int
	keys          fleetKeymap

	// problem is the source's last read failure, refreshed on every update.
	problem string

	// builder, when non-nil, causes Enter to emit a DrillDownMsg instead
	// of fetching the text detail view. The router sets this; standalone
	// fleet mode (no router) leaves it nil and gets the text detail.
	builder ClusterBuilder
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

// SetAutoSelect pins a cluster to drill into as soon as the fleet list
// arrives. This is the --server-cluster path: the fleet list still loads
// (so Esc returns to a populated screen), but the first frame shows the
// cluster's detail.
func (m *FleetModel) SetAutoSelect(clusterID string) {
	m.autoSelect = clusterID
}

// SetBuilder enables dashboard drill-down. When set, Enter emits a
// DrillDownMsg for the router to catch; when nil, Enter falls back to
// the text detail view.
func (m *FleetModel) SetBuilder(b ClusterBuilder) {
	m.builder = b
}

// FleetUpdateMsg carries a fresh fleet view. Exported so the render path
// can populate the model without running the event loop.
type FleetUpdateMsg struct {
	Clusters []fleet.ClusterView
	Storage  fleet.Storage
	// Problem is why the read failed, or empty. Carried with the data rather
	// than read separately so the two cannot disagree about which read they
	// describe.
	Problem string
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
		return FleetUpdateMsg{
			Clusters: src.View(),
			Storage:  src.Storage(),
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

	case FleetUpdateMsg:
		m.clusters = m.sortClusters(msg.Clusters)
		m.storage = msg.Storage
		m.problem = msg.Problem
		if m.selected >= len(m.filteredClusters()) {
			m.selected = max(len(m.filteredClusters())-1, 0)
		}
		// --server-cluster: drill into the named cluster as soon as the
		// fleet list arrives. The list is still loaded so Esc returns to
		// a populated screen.
		if m.autoSelect != "" {
			for _, c := range m.clusters {
				if c.Namespace+"/"+c.Name == m.autoSelect {
					m.autoSelect = ""
					return m, fetchCluster(m.source, c.Namespace, c.Name)
				}
			}
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
		if m.filtering {
			return m.handleFilterKey(msg)
		}
		return m.handleFleetKey(msg)
	}
	return m, nil
}

// sortClusters orders clusters worst-first (by status severity, then
// namespace, then name). When reversed, the order flips so the healthy
// end leads — for scanning which clusters are fine.
func (m *FleetModel) sortClusters(views []fleet.ClusterView) []fleet.ClusterView {
	out := append([]fleet.ClusterView(nil), views...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status > out[j].Status
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	if m.reversed {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
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
	case key.Matches(msg, m.keys.Reverse):
		m.reversed = !m.reversed
		m.clusters = m.sortClusters(m.clusters)
		return m, nil
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		return m, nil
	case key.Matches(msg, m.keys.Up):
		if m.selected > 0 {
			m.selected--
		}
	case key.Matches(msg, m.keys.Down):
		if m.selected < len(m.filteredClusters())-1 {
			m.selected++
		}
	case key.Matches(msg, m.keys.Enter):
		fc := m.filteredClusters()
		if len(fc) > 0 && m.selected < len(fc) {
			c := fc[m.selected]
			if m.builder != nil {
				return m, func() tea.Msg {
					return DrillDownMsg{Namespace: c.Namespace, Name: c.Name}
				}
			}
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
	used := lipgloss.Height(header) + lipgloss.Height(body) + lipgloss.Height(footer)
	if m.filtering {
		filterLine := tui.StyleMuted.Render("/ ") + m.filter + tui.StyleMuted.Render("▏")
		lines = append(lines, filterLine)
		used += lipgloss.Height(filterLine)
	}
	if h := m.height - used; h > 0 {
		lines = append(lines, strings.Repeat("\n", h-1))
	}
	lines = append(lines, footer)

	result := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return padLines(result, m.width)
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
	clusters := m.filteredClusters()
	if len(clusters) == 0 {
		switch {
		case m.problem != "":
			// Not "waiting for data": nothing is coming, and saying so is the
			// difference between an operator retrying and an operator
			// believing the fleet is empty.
			return tui.StyleErr.Render(m.problem)
		case m.filter != "":
			return tui.StyleMuted.Render("no clusters match \"" + m.filter + "\"")
		default:
			return tui.StyleMuted.Render("waiting for data…")
		}
	}

	var body string
	if m.problem != "" {
		// Clusters *and* a problem means what is on screen is the last good
		// read. It is still worth showing — stale numbers beat none — but not
		// worth showing silently.
		body += tui.StyleWarn.Render("stale: "+m.problem) + "\n"
	}

	header := fmt.Sprintf("%-3s  %-30s  %-10s  %-8s  %s",
		"", "CLUSTER", "VERSION", "NODES", "PROBLEM")
	body += tui.StyleMuted.Render(header) + "\n"

	availH := m.height - 4 // header(2) + footer(1) + column header(1)
	if availH < 1 {
		availH = 1
	}

	end := min(m.selected+availH, len(clusters))
	start := max(0, end-availH)

	for i := start; i < end; i++ {
		c := clusters[i]
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

		// One style carries both decisions. Selection used to build its own
		// from scratch, which discarded the grounded background a line above
		// and left those themes rendering the selected row on the terminal's
		// own color.
		style := lipgloss.NewStyle()
		if th.Grounded() {
			style = style.Background(th.PaneBG)
		}
		if i == m.selected {
			style = style.Bold(true)
		}

		row := style.Render(fmt.Sprintf("%s %-2s  %-30s  %-10s  %-8s  %s",
			marker, statusGlyph, name, version, nodes, problem))

		body += row + "\n"
	}

	return strings.TrimRight(body, "\n")
}

func (m *FleetModel) fleetFooter(th tui.Theme) string {
	status := styleStatusBar()
	clusters := m.filteredClusters()
	count := fmt.Sprintf("%d clusters", len(clusters))
	if len(m.clusters) != len(clusters) {
		count = fmt.Sprintf("%d/%d clusters", len(clusters), len(m.clusters))
	}
	if len(m.storage.Clusters) > 0 {
		count += fmt.Sprintf(" · %d storage clusters", len(m.storage.Clusters))
	}
	hint := "? help · ↑↓ navigate · enter detail · r reverse · / filter · q quit"
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
	fmt.Fprintf(&sb, "Status:   %s\n", statusText(d.Status))
	if d.Version != "" {
		fmt.Fprintf(&sb, "Version:  %s\n", d.Version)
	}
	if d.Phase != "" {
		fmt.Fprintf(&sb, "Phase:    %s\n", d.Phase)
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
			fmt.Fprintf(&sb, "  %-30s  %s  %d/%d  %s\n",
				p.Name, p.Role, p.Ready, p.Desired, p.Version)
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
		fmt.Fprintf(&sb, "\nMachines: %d shown, %d total\n",
			len(d.Machines.Shown), d.Machines.Total())
	}

	// Hosts summary
	if d.Hosts.Total() > 0 {
		fmt.Fprintf(&sb, "Hosts:    %d shown, %d total", len(d.Hosts.Shown), d.Hosts.Total())
		if d.HostsElsewhere > 0 {
			fmt.Fprintf(&sb, " (%d elsewhere)", d.HostsElsewhere)
		}
		sb.WriteString("\n")
	}

	// Nodes summary
	if d.NodeRows.Total() > 0 {
		fmt.Fprintf(&sb, "Nodes:    %d shown, %d total\n",
			len(d.NodeRows.Shown), d.NodeRows.Total())
	}

	// Events summary
	if d.EventsTotal > 0 {
		fmt.Fprintf(&sb, "Events:   %d groups, %d total", len(d.Events.Shown), d.EventsTotal)
		if d.EventsTruncated > 0 {
			fmt.Fprintf(&sb, " (%d truncated)", d.EventsTruncated)
		}
		sb.WriteString("\n")
	}

	// Updated
	if !d.UpdatedAt.IsZero() {
		age := time.Since(d.UpdatedAt).Round(time.Second)
		fmt.Fprintf(&sb, "\nUpdated %s ago\n", age)
	}

	// Footer
	footer := "\n" + tui.StyleMuted.Render("esc back · q quit")
	sb.WriteString(footer)

	return padLines(sb.String(), m.width)
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

// statusText renders a status with the active theme's colors. It takes no
// theme: the styles are package-level and reassigned when a theme is applied.
func statusText(s health.Status) string {
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

// padLines ensures every line of a multi-line string is exactly width columns,
// truncating wide lines and padding narrow ones. This is the multi-line
// counterpart to padToWidth, which only handles a single line.
func padLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = padToWidth(ln, width)
	}
	return strings.Join(lines, "\n")
}

// fleetKeymap is the key set for the fleet model.
type fleetKeymap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Back    key.Binding
	Quit    key.Binding
	Help    key.Binding
	Reverse key.Binding
	Filter  key.Binding
}

func defaultFleetKeymap() fleetKeymap {
	return fleetKeymap{
		Up:      key.NewBinding(key.WithKeys("up", "k")),
		Down:    key.NewBinding(key.WithKeys("down", "j")),
		Enter:   key.NewBinding(key.WithKeys("enter")),
		Back:    key.NewBinding(key.WithKeys("esc", "backspace")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c")),
		Help:    key.NewBinding(key.WithKeys("?")),
		Reverse: key.NewBinding(key.WithKeys("r")),
		Filter:  key.NewBinding(key.WithKeys("/")),
	}
}

// handleFilterKey processes keystrokes while the filter input is active.
// Backspace edits, enter applies, esc clears and exits filter mode.
func (m *FleetModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter = ""
		m.filtering = false
		m.clampSelected()
		return m, nil
	case tea.KeyEnter:
		m.filtering = false
		m.clampSelected()
		return m, nil
	case tea.KeyBackspace:
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
		}
		if m.filter == "" {
			m.filtering = false
		}
		m.clampSelected()
		return m, nil
	case tea.KeyRunes:
		m.filter += string(msg.Runes)
		m.clampSelected()
		return m, nil
	}
	return m, nil
}

// clampSelected keeps the selection within the filtered list's bounds.
func (m *FleetModel) clampSelected() {
	if m.selected >= len(m.filteredClusters()) {
		m.selected = max(len(m.filteredClusters())-1, 0)
	}
}

// filteredClusters returns the clusters matching the current filter, or
// all clusters when no filter is set.
func (m *FleetModel) filteredClusters() []fleet.ClusterView {
	if m.filter == "" {
		return m.clusters
	}
	needle := strings.ToLower(m.filter)
	var out []fleet.ClusterView
	for _, c := range m.clusters {
		if strings.Contains(strings.ToLower(c.Name), needle) ||
			strings.Contains(strings.ToLower(c.Namespace), needle) {
			out = append(out, c)
		}
	}
	return out
}
