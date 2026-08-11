package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// NodesPane lists the workload cluster's nodes with readiness, version, and
// resource commitment.
//
// Version is shown per node because during a rollout a mixed-version node list
// *is* the progress report, seen from the workload cluster's side rather than
// Cluster API's.
type NodesPane struct {
	base
	store *store.Store
	roles profile.NodeRoles
	// targetVersion, when set, highlights nodes not yet on it.
	targetVersion string
}

// NewNodes builds the nodes pane.
func NewNodes(s *store.Store, roles profile.NodeRoles, targetVersion string) *NodesPane {
	return &NodesPane{
		base: base{
			id: "nodes", title: "Nodes",
			priority: tui.P0Critical, minW: 46, minH: 8, weight: 4,
			// Two rows, for the same reason as Machines & Hosts: node count is the
			// cluster's size, and width does nothing for it.
			rows: 2,
		},
		store:         s,
		roles:         roles,
		targetVersion: targetVersion,
	}
}

var (
	nodeColsFull = []table.Column{
		{Header: "NODE", Stretch: true},
		{Header: "ROLE"},
		{Header: "STATUS"},
		{Header: "VERSION"},
		{Header: "CPU"},
		{Header: "MEM"},
		{Header: "AGE"},
	}
	nodeColsMedium = []table.Column{
		{Header: "NODE", Stretch: true},
		{Header: "STATUS"},
		{Header: "VERSION"},
		{Header: "CPU"},
		{Header: "MEM"},
	}
	nodeColsNarrow = []table.Column{
		{Header: "NODE", Stretch: true},
		{Header: "STATUS"},
		{Header: "VERSION"},
	}
)

// nodeColumns picks a column set by width, dropping the least-valuable first.
// Version stays in every set: a mixed-version node list is the rollout seen from
// the workload cluster's side, which is the reason to look at this pane at all.
func nodeColumns(w int) ([]table.Column, []int) {
	switch {
	case w >= 76:
		return nodeColsFull, []int{0, 1, 2, 3, 4, 5, 6}
	case w >= 52:
		return nodeColsMedium, []int{0, 2, 3, 4, 5}
	default:
		return nodeColsNarrow, []int{0, 2, 3}
	}
}

// rows builds the node cells and their styles.
//
// Shared by Render and ContentWidth, so the pane's declared appetite comes from
// the same cells it draws — see [MachinesPane.fleet].
func (p *NodesPane) rows(items []model.Node) (cells [][]string, styles [][]lipgloss.Style) {
	cells = make([][]string, 0, len(items))
	styles = make([][]lipgloss.Style, 0, len(items))
	for _, n := range items {
		cells = append(cells, []string{
			n.Name,
			orDash(p.roles.DisplayName(n.Role)),
			n.DisplayStatus(),
			orDash(n.Version),
			pct(n.RequestedCPU, n.AllocatableCPU),
			pct(n.RequestedMemory, n.AllocatableMemory),
			table.ShortAge(n.Age.Seconds()),
		})
		styles = append(styles, []lipgloss.Style{
			{}, {},
			p.statusStyle(n),
			p.versionStyle(n.Version),
			pctStyle(n.RequestedCPU, n.AllocatableCPU),
			pctStyle(n.RequestedMemory, n.AllocatableMemory),
			tui.StyleMuted,
		})
	}
	return cells, styles
}

// ContentWidth implements [tui.ContentWidthPane], from the full column set: a
// node's FQDN is the cell a reader identifies the row by, and it is the first
// thing a narrow tile takes away.
func (p *NodesPane) ContentWidth() int {
	snap, ok := store.Get[model.Snapshot[model.Node]](p.store, model.KeyWorkloadNodes)
	if !ok || len(snap.Items) == 0 {
		return 0
	}
	cells, _ := p.rows(snap.Items)
	return table.AppetiteWidth(nodeColsFull, cells)
}

// Render implements tui.Pane.
func (p *NodesPane) Render(w, h int, _ bool) string {
	snap, body, ok := snapshotOf[model.Node](p.store, model.KeyWorkloadNodes, w, h, "nodes")
	if !ok {
		return body
	}
	if len(snap.Items) == 0 {
		return table.Placeholder(w, h, "no nodes")
	}

	cells, styles := p.rows(snap.Items)
	cols, keep := nodeColumns(w)
	t := table.Table{
		Cols:       cols,
		Rows:       selectCells(cells, keep),
		CellStyles: selectStyles(styles, keep),
	}
	total := p.totalLine(snap.Items, w)
	if total == "" || h < len(cells)+3 {
		return t.Render(w, h)
	}
	return clipTo(t.Render(w, h-2)+"\n\n"+total, w, h)
}

// statusStyle colors a node's status cell.
//
// Two things the composed status token cannot express on its own:
//
//   - A node that is cordoned *and* not Ready must read as the failure it is.
//     Styling the composed "NotReady,Cordoned" would fall through to muted —
//     "no opinion" — for a node that is down, so the underlying condition is
//     styled instead.
//   - A cordon the profile declares expected for this role is healthy, not a
//     warning. The cell still says Cordoned, because that is true and worth
//     knowing; it just stops being amber.
func (p *NodesPane) statusStyle(n model.Node) lipgloss.Style {
	if !n.Ready() {
		return tui.StatusStyle(n.Status)
	}
	if n.Cordoned && !p.roles.CordonIsNews(n.Role, true) {
		return tui.StyleOK
	}
	return tui.StatusStyle(n.DisplayStatus())
}

// versionStyle highlights a node that has not reached the target version.
//
// Without a target there is nothing to compare against, so every version renders
// neutrally rather than guessing which is newest — string-comparing semantic
// versions gets "v1.10.0" wrong against "v1.9.0".
func (p *NodesPane) versionStyle(version string) lipgloss.Style {
	if p.targetVersion == "" || version == "" {
		return lipgloss.Style{}
	}
	if strings.Contains(version, p.targetVersion) {
		return tui.StyleOK
	}
	return tui.StyleWarn
}

// totalLine summarizes capacity across every node, so the pane answers "is there
// room" as well as "is anything down".
func (p *NodesPane) totalLine(nodes []model.Node, w int) string {
	var cpuReq, cpuAlloc, memReq, memAlloc int64
	ready, cordoned := 0, 0
	for _, n := range nodes {
		cpuReq += n.RequestedCPU
		cpuAlloc += n.AllocatableCPU
		memReq += n.RequestedMemory
		memAlloc += n.AllocatableMemory
		if n.Ready() {
			ready++
		}
		// Only count a cordon the profile has not declared expected for this
		// role; see profile.NodeRoles.CordonExpected.
		if n.Cordoned && p.roles.CordonIsNews(n.Role, n.Ready()) {
			cordoned++
		}
	}
	if cpuAlloc == 0 && memAlloc == 0 {
		return ""
	}

	parts := []string{
		countStyle(int32(ready), int32(len(nodes))).Render(fmt.Sprintf("%d/%d ready", ready, len(nodes))),
	}
	// A truncated total line is worse than a terse one, so drop the absolute
	// figures when they will not fit and keep the percentages.
	if w >= 72 {
		parts = append(parts,
			fmt.Sprintf("cpu %s/%s (%s)", millicores(cpuReq), millicores(cpuAlloc),
				pctStyle(cpuReq, cpuAlloc).Render(pct(cpuReq, cpuAlloc))),
			fmt.Sprintf("mem %s/%s (%s)", humanBytes(memReq), humanBytes(memAlloc),
				pctStyle(memReq, memAlloc).Render(pct(memReq, memAlloc))))
	} else {
		parts = append(parts,
			"cpu "+pctStyle(cpuReq, cpuAlloc).Render(pct(cpuReq, cpuAlloc)),
			"mem "+pctStyle(memReq, memAlloc).Render(pct(memReq, memAlloc)))
	}
	if cordoned > 0 {
		parts = append(parts, tui.StyleWarn.Render(fmt.Sprintf("%d cordoned", cordoned)))
	}
	return tui.StyleHeader.Render("Cluster: ") + strings.Join(parts, "  ")
}
