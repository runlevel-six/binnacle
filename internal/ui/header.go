package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/rollout"
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

	// The version rides on the name, dim and without a separator, so it reads as
	// a stamp on the title rather than a fourth labeled field competing with the
	// cluster identity beside it. It sits leftmost because that end of the row is
	// the stable one: truncation eats the right, and the rollout and FROZEN
	// badges claim that edge when they appear.
	title := hs.title.Render(th.Label("sextant"))
	if v := m.buildInfo.Short(); v != "" {
		title += hs.dim.Render(" " + v)
	}

	parts := []string{title}
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
		parts = append(parts, tui.RenderCell(c, hs.dim))
	}
	joined := strings.Join(parts, gap)
	for i := len(parts) - 1; i > 0 && lipgloss.Width(joined) > width; i-- {
		joined = strings.Join(parts[:i], gap)
	}
	return joined
}

// bannerCells builds the core health cells, then appends any a plugin
// contributed.
//
// The core verdicts live in pkg/health rather than here so that a consumer
// outside the terminal reaches the same conclusions from the same store. Two
// front ends disagreeing about whether a cluster is healthy would be worse than
// either of them simply being wrong.
func (m *Model) bannerCells() []tui.BannerCell {
	cells := health.CoreCells(m.store, m.resolved.Profile.NodeRoles)
	if m.registry != nil {
		cells = append(cells, m.registry.BannerCells(m.store)...)
	}
	return cells
}
