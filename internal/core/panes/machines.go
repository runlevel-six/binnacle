package panes

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/core/capi"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// MachinesPane shows each Cluster API Machine joined to the physical host
// beneath it, plus the hosts nobody has claimed.
//
// This is the pane that justifies the tool. During a rolling upgrade the
// question is always "which box is this, and what is it doing" — and answering
// it otherwise means correlating `clusterctl describe` against two more
// `kubectl get` invocations by hand.
type MachinesPane struct {
	base
	store *store.Store
	roles profile.NodeRoles
}

// NewMachines builds the machines pane.
func NewMachines(s *store.Store, roles profile.NodeRoles) *MachinesPane {
	return &MachinesPane{
		base: base{
			id: "machines", title: "Machines & Hosts",
			priority: tui.P0Critical, minW: 52, minH: 8, weight: 4,
			// Two rows: a 54-machine fleet renders 25 and "+ 29 more" at any width,
			// because the limit is rows and not columns.
			rows: 2,
		},
		store: s,
		roles: roles,
	}
}

// Column sets by available width.
//
// Seven columns squeezed into a narrow tile truncates every one of them, which
// makes the table useless — "prod-cp-" and "rack1-no" tell a reader nothing.
// Dropping whole columns keeps the remaining ones legible, and the order here is
// least-valuable-first: age and version are recoverable elsewhere, but a machine
// name and the host it maps to are the pane's entire point.
var (
	machineColsFull = []table.Column{
		{Header: "MACHINE"},
		{Header: "ROLE"},
		{Header: "PHASE"},
		{Header: "VERSION"},
		{Header: "HOST"},
		{Header: "HOST STATE", Stretch: true},
		{Header: "AGE"},
	}
	machineColsMedium = []table.Column{
		{Header: "MACHINE"},
		{Header: "ROLE"},
		{Header: "PHASE"},
		{Header: "HOST"},
		{Header: "HOST STATE", Stretch: true},
	}
	machineColsNarrow = []table.Column{
		{Header: "MACHINE"},
		{Header: "PHASE"},
		{Header: "HOST STATE", Stretch: true},
	}
)

// machineColumns picks a column set and the indices of the full row to keep.
func machineColumns(w int) ([]table.Column, []int) {
	switch {
	case w >= 86:
		return machineColsFull, []int{0, 1, 2, 3, 4, 5, 6}
	case w >= 58:
		return machineColsMedium, []int{0, 1, 2, 4, 5}
	default:
		return machineColsNarrow, []int{0, 2, 5}
	}
}

// selectCells projects a full row down to the chosen columns.
func selectCells(rows [][]string, keep []int) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, 0, len(keep))
		for _, i := range keep {
			if i < len(r) {
				row = append(row, r[i])
			}
		}
		out = append(out, row)
	}
	return out
}

// selectStyles is selectCells for the parallel style slices.
func selectStyles(rows [][]lipgloss.Style, keep []int) [][]lipgloss.Style {
	out := make([][]lipgloss.Style, 0, len(rows))
	for _, r := range rows {
		row := make([]lipgloss.Style, 0, len(keep))
		for _, i := range keep {
			if i < len(r) {
				row = append(row, r[i])
			}
		}
		out = append(out, row)
	}
	return out
}

// Render implements tui.Pane.
func (p *MachinesPane) Render(w, h int, _ bool) string {
	machines, body, ok := snapshotOf[model.Machine](p.store, model.KeyMgmtMachines, w, h, "machines")
	if !ok {
		return body
	}
	// Metal3 may legitimately be absent; the Machine list is still worth showing
	// with empty host columns, so these are read without failing the pane.
	m3ms, _ := store.Get[model.Snapshot[model.Metal3Machine]](p.store, model.KeyMgmtMetal3Machines)
	bmhs, _ := store.Get[model.Snapshot[model.BareMetalHost]](p.store, model.KeyMgmtBareMetalHosts)

	if len(machines.Items) == 0 {
		return table.Placeholder(w, h, "no Cluster API machines")
	}

	rows := capi.Join(machines.Items, m3ms.Items, bmhs.Items, p.roleOf)
	unclaimed := capi.UnclaimedHosts(m3ms.Items, bmhs.Items)

	allCells, allStyles := machineRows(rows, p.roles)
	cols, keep := machineColumns(w)
	cells := selectCells(allCells, keep)
	main := table.Table{Cols: cols, Rows: cells, CellStyles: selectStyles(allStyles, keep)}

	// The unclaimed-host summary earns its line only when the main table is not
	// already using every row: a machine hidden behind a footer is worse than an
	// absent summary.
	summary := unclaimedSummary(unclaimed)
	if summary == "" || h < len(cells)+3 {
		return main.Render(w, h)
	}
	return clipTo(main.Render(w, h-2)+"\n\n"+summary, w, h)
}

// roleOf derives a worker Machine's role from the MachineSet name in its
// ownerReference.
//
// Cluster API names a MachineSet after its MachineDeployment plus a hash, so the
// deployment's name is a prefix of it. That is the only link back to the pool
// without watching MachineSets as well, which would be a third informer for one
// string.
func (p *MachinesPane) roleOf(m model.Machine) string {
	if role := p.roles.RoleFromMachineDeploymentName(m.OwnerName); role != "" {
		return role
	}
	return p.roles.RoleFromMachineDeploymentName(m.Name)
}

func machineRows(rows []capi.HostRow, roles profile.NodeRoles) ([][]string, [][]lipgloss.Style) {
	cells := make([][]string, 0, len(rows))
	styles := make([][]lipgloss.Style, 0, len(rows))

	for _, r := range rows {
		host, hostState := "—", tui.StyleMuted.Render("unbound")
		hostStyle := tui.StyleMuted
		if b := r.BareMetalHost; b != nil {
			host = b.Name
			hostState = hostStateText(*b)
			hostStyle = hostStateStyle(*b)
		} else if r.Metal3Machine != nil {
			// A provider machine exists but no host is bound: the interesting
			// distinction from "no provider machine at all", because it usually
			// means no host matched.
			hostState = tui.StyleWarn.Render("awaiting host")
			hostStyle = tui.StyleWarn
		}

		cells = append(cells, []string{
			r.Machine.Name,
			roleLabel(roles, r.Role),
			orDash(r.Machine.Phase),
			orDash(r.Machine.Version),
			host,
			hostState,
			table.ShortAge(r.Machine.Age.Seconds()),
		})
		styles = append(styles, []lipgloss.Style{
			{}, {},
			tui.StatusStyle(r.Machine.Phase),
			{}, {},
			hostStyle,
			tui.StyleMuted,
		})
	}
	return cells, styles
}

// hostStateText combines provisioning state with the exceptions worth seeing at
// a glance: an error, or a host that is powered off while claimed.
func hostStateText(b model.BareMetalHost) string {
	parts := []string{orDash(b.State)}
	if b.ErrorMessage != "" {
		parts = append(parts, tui.StyleErr.Render(b.ErrorMessage))
	} else if b.ConsumerName != "" && !b.PoweredOn {
		parts = append(parts, tui.StyleWarn.Render("powered off"))
	}
	return strings.Join(parts, " ")
}

// hostStateStyle colors a host by its provisioning state.
//
// The Metal3 state machine has many states; what matters to a reader is whether
// the host is settled, moving, or broken.
func hostStateStyle(b model.BareMetalHost) lipgloss.Style {
	if b.ErrorMessage != "" {
		return tui.StyleErr
	}
	switch b.State {
	case "provisioned", "available":
		return tui.StyleOK
	case "provisioning", "deprovisioning", "inspecting", "registering", "preparing", "matchprofile":
		return tui.StyleWarn
	case "":
		return tui.StyleMuted
	}
	// An unrecognized state is reported neutrally rather than as an alarm: the
	// Metal3 state machine gains states over time, and guessing wrong would
	// either cry wolf or hide a real problem.
	return tui.StyleMuted
}

// unclaimedSummary describes the hosts no machine is using: the fleet's spare
// capacity, and where a failed scale-up shows up first.
func unclaimedSummary(hosts []model.BareMetalHost) string {
	if len(hosts) == 0 {
		return ""
	}
	byState := map[string]int{}
	var order []string
	errored := 0
	for _, b := range hosts {
		if b.ErrorMessage != "" {
			errored++
		}
		st := b.State
		if st == "" {
			st = "unknown"
		}
		if _, seen := byState[st]; !seen {
			order = append(order, st)
		}
		byState[st]++
	}

	parts := make([]string, 0, len(order))
	for _, st := range order {
		text := fmt.Sprintf("%d %s", byState[st], st)
		if st == "available" {
			parts = append(parts, tui.StyleOK.Render(text))
		} else {
			parts = append(parts, tui.StyleMuted.Render(text))
		}
	}
	line := tui.StyleHeader.Render(fmt.Sprintf("Unclaimed hosts (%d): ", len(hosts))) +
		strings.Join(parts, ", ")
	if errored > 0 {
		line += "  " + tui.StyleErr.Render(fmt.Sprintf("%d with errors", errored))
	}
	return line
}

// roleLabel renders a role through the profile's display names, falling back to
// an em dash when a machine's pool could not be determined.
func roleLabel(roles profile.NodeRoles, role string) string {
	if role == "" {
		return "—"
	}
	return roles.DisplayName(role)
}
