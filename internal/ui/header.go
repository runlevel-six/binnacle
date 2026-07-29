package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/core/rollout"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// headerStyles are the styles the header rows are assembled from.
//
// Every one carries the bar's background, including the one for plain text.
// That is not redundancy: a nested style's reset sequence clears the background
// as well as its own color, and lipgloss does not re-arm the enclosing style
// afterwards — so a bar wrapped around styled spans comes out with holes
// punched through it wherever one sits.
type headerStyles struct {
	bar   lipgloss.Style
	title lipgloss.Style
	dim   lipgloss.Style
	warn  lipgloss.Style
	err   lipgloss.Style
}

func newHeaderStyles() headerStyles {
	th := tui.CurrentTheme()
	on := func(s lipgloss.Style) lipgloss.Style { return s.Background(th.HeaderBG) }
	return headerStyles{
		bar:   on(lipgloss.NewStyle().Foreground(th.HeaderFG)),
		title: on(styleTitle()),
		dim:   on(styleTitleDim()),
		warn:  on(tui.StyleWarn),
		err:   on(tui.StyleErr),
	}
}

// renderHeader draws the two-row header: an identity line, then the health strip.
func (m *Model) renderHeader(width int) string {
	hs := newHeaderStyles()
	return strings.Join([]string{
		barRow(hs, m.identityLine(hs, width-2), width),
		barRow(hs, m.bannerLine(hs, width-2), width),
	}, "\n")
}

// barRow lays content on a band of exactly width cells, indented one cell and
// padded out in the bar's own style so the band is solid edge to edge.
func barRow(hs headerStyles, content string, width int) string {
	row := hs.bar.Render(" ") + content
	if pad := width - lipgloss.Width(row); pad > 0 {
		row += hs.bar.Render(strings.Repeat(" ", pad))
	}
	return truncate(row, width)
}

// identityLine names what is being watched and the current mode.
//
// Which cluster is on screen is the first thing to be sure of, since acting on
// the wrong one during a maintenance window is the expensive mistake this tool
// exists to prevent.
//
// Labels go through the theme, which may shout them; the context, profile and
// version names beside them never do. Those are identifiers, and a theme has no
// business rewriting one.
func (m *Model) identityLine(hs headerStyles, width int) string {
	th := tui.CurrentTheme()
	st := rollout.Detect(m.store, m.resolved.TargetVersion)

	parts := []string{hs.title.Render(th.Label("sextant"))}
	label := func(name, value string) string {
		return hs.bar.Render(th.Label(name)+" ") + hs.title.Render(value)
	}
	if m.resolved.WorkloadIsManagement {
		parts = append(parts, label("cluster", m.resolved.ManagementContext))
	} else {
		parts = append(parts,
			label("mgmt", m.resolved.ManagementContext),
			label("workload", m.resolved.WorkloadContext),
		)
	}
	if name := m.resolved.Profile.Name; name != "" {
		parts = append(parts, hs.bar.Render(th.Label("profile")+" "+name))
	}
	switch {
	case st.Active && st.Asserted:
		parts = append(parts, hs.warn.Render("ROLLOUT → "+st.TargetVersion))
	case st.Active:
		parts = append(parts, hs.warn.Render(fmt.Sprintf("ROLLOUT (%d pool)", len(st.Rolling))))
	}
	if m.paused {
		parts = append(parts, hs.err.Render("FROZEN"))
	}
	return truncate(strings.Join(parts, hs.dim.Render(th.Separator)), width)
}

// bannerLine renders the health strip: one cell per subsystem.
//
// Cells are dropped from the right when the row overflows, so the leftmost —
// the Cluster API and node health that matter most — survive at any width.
func (m *Model) bannerLine(hs headerStyles, width int) string {
	cells := m.bannerCells()
	if len(cells) == 0 {
		return hs.dim.Render("waiting for data…")
	}

	gap := hs.bar.Render("  ")
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		parts = append(parts, c.Render(hs.dim))
	}
	joined := strings.Join(parts, gap)
	for i := len(parts) - 1; i > 0 && lipgloss.Width(joined) > width; i-- {
		joined = strings.Join(parts[:i], gap)
	}
	return joined
}

// bannerCells builds the core health cells, then appends any a plugin
// contributed.
func (m *Model) bannerCells() []tui.BannerCell {
	var cells []tui.BannerCell
	for _, build := range []func() (tui.BannerCell, bool){
		m.cellRollout, m.cellMachines, m.cellHosts, m.cellNodes, m.cellPods,
	} {
		if c, ok := build(); ok {
			cells = append(cells, c)
		}
	}
	if m.registry != nil {
		cells = append(cells, m.registry.BannerCells(m.store)...)
	}
	return cells
}

// cellRollout summarizes Cluster API rollout progress.
func (m *Model) cellRollout() (tui.BannerCell, bool) {
	kcps, ok := store.Get[model.Snapshot[model.KubeadmControlPlane]](m.store, model.KeyMgmtKCPs)
	mds, mdOK := store.Get[model.Snapshot[model.MachineDeployment]](m.store, model.KeyMgmtMachineDeployments)
	if (!ok || kcps.Err != nil) && (!mdOK || mds.Err != nil) {
		return tui.BannerCell{}, false
	}

	var desired, upToDate int32
	for _, k := range kcps.Items {
		desired += k.DesiredReplicas
		upToDate += k.UpToDateReplicas
	}
	for _, d := range mds.Items {
		desired += d.DesiredReplicas
		upToDate += d.UpToDateReplicas
	}

	cell := tui.BannerCell{Name: "CAPI"}
	switch {
	case desired == 0:
		cell.Status = tui.BannerLoading
	case upToDate >= desired:
		cell.Status = tui.BannerOK
	default:
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d/%d", upToDate, desired)
	}
	return cell, true
}

// cellMachines reports Machines not in a running phase.
func (m *Model) cellMachines() (tui.BannerCell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Machine]](m.store, model.KeyMgmtMachines)
	if !ok || snap.Err != nil {
		return tui.BannerCell{}, false
	}

	cell := tui.BannerCell{Name: "Machines"}
	if len(snap.Items) == 0 {
		cell.Status = tui.BannerLoading
		return cell, true
	}
	notRunning := 0
	for _, mc := range snap.Items {
		if mc.Phase != "Running" {
			notRunning++
		}
	}
	switch notRunning {
	case 0:
		cell.Status = tui.BannerOK
	default:
		// Machines transition through non-running phases during any normal
		// rollout, so this is amber rather than red.
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d/%d moving", notRunning, len(snap.Items))
	}
	return cell, true
}

// cellHosts reports BareMetalHost errors, the failure a rollout most often
// stalls on.
func (m *Model) cellHosts() (tui.BannerCell, bool) {
	snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](m.store, model.KeyMgmtBareMetalHosts)
	if !ok || snap.Err != nil {
		return tui.BannerCell{}, false
	}

	cell := tui.BannerCell{Name: "Hosts"}
	if len(snap.Items) == 0 {
		cell.Status = tui.BannerLoading
		return cell, true
	}
	errored := 0
	for _, b := range snap.Items {
		if b.ErrorMessage != "" || b.OperationalStatus == "error" {
			errored++
		}
	}
	switch errored {
	case 0:
		cell.Status = tui.BannerOK
	default:
		cell.Status = tui.BannerErr
		cell.Detail = fmt.Sprintf("%d errored", errored)
	}
	return cell, true
}

func (m *Model) cellNodes() (tui.BannerCell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Node]](m.store, model.KeyWorkloadNodes)
	if !ok || snap.Err != nil {
		return tui.BannerCell{}, false
	}

	cell := tui.BannerCell{Name: "Nodes"}
	if len(snap.Items) == 0 {
		cell.Status = tui.BannerLoading
		return cell, true
	}
	roles := m.resolved.Profile.NodeRoles
	notReady, cordoned := 0, 0
	for _, n := range snap.Items {
		if !n.Ready() {
			notReady++
		}
		// A cordon the profile declares expected for this role is the steady
		// state, not a drain, and must not hold the banner at amber for the life
		// of the cluster.
		if n.Cordoned && roles.CordonIsNews(n.Role, n.Ready()) {
			cordoned++
		}
	}
	switch {
	case notReady > 0:
		cell.Status = tui.BannerErr
		cell.Detail = fmt.Sprintf("%d NotReady", notReady)
	case cordoned > 0:
		// Cordoned is expected mid-drain, so it is a warning rather than a
		// failure.
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d cordoned", cordoned)
	default:
		cell.Status = tui.BannerOK
	}
	return cell, true
}

func (m *Model) cellPods() (tui.BannerCell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Pod]](m.store, model.KeyWorkloadPods)
	if !ok || snap.Err != nil {
		return tui.BannerCell{}, false
	}

	cell := tui.BannerCell{Name: "Pods"}
	if len(snap.Items) == 0 {
		cell.Status = tui.BannerLoading
		return cell, true
	}
	unhealthy := 0
	for _, p := range snap.Items {
		if !p.IsHealthy {
			unhealthy++
		}
	}
	switch unhealthy {
	case 0:
		cell.Status = tui.BannerOK
	default:
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d unhealthy", unhealthy)
	}
	return cell, true
}
