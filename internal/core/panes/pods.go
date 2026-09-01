package panes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// PodHealthPane shows the profile's pinned critical workloads, then every
// unhealthy pod.
//
// The two blocks answer different questions. Critical workloads are shown whether
// healthy or not, because their absence is the thing you want to notice
// immediately — an unhealthy-only list says nothing when a StatefulSet has been
// scaled to zero or deleted outright. The unhealthy list is the open-ended half.
type PodHealthPane struct {
	base
	store    *store.Store
	critical []profile.CriticalWorkload
}

// NewPodHealth builds the pod-health pane.
func NewPodHealth(s *store.Store, critical []profile.CriticalWorkload) *PodHealthPane {
	return &PodHealthPane{
		base: base{
			id: "pods", title: "Pod Health",
			priority: tui.P0Critical, minW: 46, minH: 8, weight: 7,
			// Two columns: the POD cell carries namespace/name, and at a quarter
			// of a wide terminal three different rook-ceph pods all render as
			// "rook-ceph/rook-c" — rows a reader cannot tell apart.
			span: 2,
		},
		store:    s,
		critical: critical,
	}
}

var (
	criticalCols = []table.Column{
		{Header: "WORKLOAD", Stretch: true},
		{Header: "KIND"},
		{Header: "READY"},
		{Header: "STATE"},
	}
	unhealthyCols = []table.Column{
		{Header: "POD", Stretch: true},
		{Header: "READY"},
		{Header: "STATUS"},
		{Header: "RESTARTS"},
		{Header: "NODE"},
		{Header: "AGE"},
	}
)

// ContentWidth implements [tui.ContentWidthPane]: the wider of the two tables
// this pane stacks, since they share one envelope and both render at the pane's
// full width.
//
// The cell that decides it is POD, which carries namespace/name — at a quarter of
// a wide terminal three different rook-ceph pods all render as "rook-ceph/rook-c",
// and rows a reader cannot tell apart are rows that are not being shown.
func (p *PodHealthPane) ContentWidth() int {
	snap, ok := store.Get[model.Snapshot[model.Pod]](p.store, model.KeyWorkloadPods)
	if !ok {
		return 0
	}
	critCells, _ := p.criticalRows(snap.Items)
	unhealthyCells, _ := unhealthyRows(unhealthyPods(snap.Items))
	if len(critCells) == 0 && len(unhealthyCells) == 0 {
		return 0
	}
	return max(
		table.AppetiteWidth(criticalCols, critCells),
		table.AppetiteWidth(unhealthyCols, unhealthyCells),
	)
}

// Render implements tui.Pane.
func (p *PodHealthPane) Render(w, h int, _ bool) string {
	snap, body, ok := snapshotOf[model.Pod](p.store, model.KeyWorkloadPods, w, h, "pods")
	if !ok {
		return body
	}

	unhealthy := unhealthyPods(snap.Items)

	// One shared left pad so the two tables' outer edges line up. Their schemas
	// differ, so the columns cannot align, but a common envelope keeps the pane
	// from looking like two unrelated widgets.
	critCells, critStyles := p.criticalRows(snap.Items)
	unhealthyCells, unhealthyStyles := unhealthyRows(unhealthy)
	pad := table.PaneLeftPad(w, criticalCols, unhealthyCols)

	var blocks []string
	if len(critCells) > 0 {
		blocks = append(blocks, table.IndentLines(
			renderInner(criticalCols, critCells, critStyles, w-pad, min(len(critCells)+1, h)), pad))
	}

	remaining := h - lineCount(strings.Join(blocks, "\n")) - 2
	switch {
	case len(unhealthy) == 0:
		blocks = append(blocks, tui.StyleOK.Render("All pods healthy"))
	case remaining >= 2:
		header := tui.StyleHeader.Render(fmt.Sprintf("Unhealthy pods (%d)", len(unhealthy)))
		blocks = append(blocks, header+"\n"+table.IndentLines(
			renderInner(unhealthyCols, unhealthyCells, unhealthyStyles, w-pad, remaining-1), pad))
	default:
		// No room for the table, but the count still has to be visible: silently
		// dropping it would read as "nothing is wrong".
		blocks = append(blocks, tui.StyleErr.Render(
			fmt.Sprintf("%d unhealthy pod(s) — widen or zoom to see them", len(unhealthy))))
	}
	return clipTo(strings.Join(blocks, "\n\n"), w, h)
}

// renderInner draws a table at a pre-computed width, bypassing the single-table
// edge padding so several tables in one pane can share a pad.
func renderInner(cols []table.Column, rows [][]string, styles [][]lipgloss.Style, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	t := table.Table{Cols: cols, Rows: rows, CellStyles: styles}
	return t.Render(w, h)
}

func (p *PodHealthPane) criticalRows(pods []model.Pod) ([][]string, [][]lipgloss.Style) {
	if len(p.critical) == 0 {
		return nil, nil
	}
	cells := make([][]string, 0, len(p.critical))
	styles := make([][]lipgloss.Style, 0, len(p.critical))

	for _, c := range p.critical {
		var matched []model.Pod
		for _, pod := range pods {
			if c.Matches(pod.Namespace, pod.Name) {
				matched = append(matched, pod)
			}
		}

		ready := 0
		for _, pod := range matched {
			if pod.IsHealthy {
				ready++
			}
		}

		state, style := "missing", tui.StyleErr
		switch {
		case len(matched) == 0:
			// A pinned workload with no pods at all is the case this block
			// exists for, and the one an unhealthy-only list cannot report.
		case ready == len(matched):
			state, style = "healthy", tui.StyleOK
		default:
			state, style = "degraded", tui.StyleWarn
		}

		cells = append(cells, []string{
			c.Namespace + "/" + c.Name,
			c.Kind,
			fmt.Sprintf("%d/%d", ready, len(matched)),
			state,
		})
		styles = append(styles, []lipgloss.Style{{}, tui.StyleMuted, style, style})
	}
	return cells, styles
}

// unhealthyPods filters and orders the pods worth attention: most restarts
// first, since a crash-looping pod is a live problem, then by name.
//
// The filter is [health.NeedsAttention], the same one the health cell counts
// with, so the pane and the cell above it cannot report different numbers.
func unhealthyPods(pods []model.Pod) []model.Pod {
	out := make([]model.Pod, 0)
	for _, p := range pods {
		if health.NeedsAttention(p) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Restarts != out[j].Restarts {
			return out[i].Restarts > out[j].Restarts
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func unhealthyRows(pods []model.Pod) ([][]string, [][]lipgloss.Style) {
	cells := make([][]string, 0, len(pods))
	styles := make([][]lipgloss.Style, 0, len(pods))
	for _, p := range pods {
		restarts := fmt.Sprintf("%d", p.Restarts)
		restartStyle := tui.StyleMuted
		if p.Restarts > 0 {
			restartStyle = tui.StyleWarn
		}
		cells = append(cells, []string{
			p.Namespace + "/" + p.Name,
			fmt.Sprintf("%d/%d", p.ReadyReady, p.ReadyTotal),
			p.Status,
			restarts,
			orDash(p.Node),
			table.ShortAge(p.Age.Seconds()),
		})
		styles = append(styles, []lipgloss.Style{
			{},
			countStyle(p.ReadyReady, p.ReadyTotal),
			tui.StatusStyle(p.Status),
			restartStyle,
			tui.StyleMuted,
			tui.StyleMuted,
		})
	}
	return cells, styles
}
