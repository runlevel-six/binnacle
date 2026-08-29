package panes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/rollout"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// OverviewPane is the top-of-screen summary: which clusters exist, how far a
// rollout has progressed, and how the workload cluster's nodes and workloads
// are holding up.
//
// It is mode-aware. During a rollout it shows per-pool progress bars, because
// the question is "how far along"; in steady state it shows ready counts,
// because the question is "is anything wrong". Showing progress bars at rest
// would be a row of full green bars conveying nothing.
type OverviewPane struct {
	base
	store *store.Store
	roles profile.NodeRoles
	// targetVersion is the operator's stated goal, threaded through so this
	// pane and the rollout detector agree on the mode.
	targetVersion string
	// widthFraction divides the terminal to size the fixed top row. Zero means
	// full width.
	widthFraction int
	// summaries supplies blocks contributed by plugins, consulted on every render
	// so a subsystem can appear the moment it has something to say. A func rather
	// than a slice, and returning [tui.SummaryBlock] rather than anything of the
	// plugins' own, because this package is core and must not import them.
	summaries func() []tui.SummaryBlock
}

// NewOverview builds the overview pane.
func NewOverview(s *store.Store, roles profile.NodeRoles, targetVersion string) *OverviewPane {
	return &OverviewPane{
		base: base{
			id: "overview", title: "Overview",
			priority: tui.P0Critical, minW: 44, minH: 8, weight: 1,
		},
		store:         s,
		roles:         roles,
		targetVersion: targetVersion,
	}
}

// WithSummaries supplies plugin-contributed blocks. See [tui.SummaryBlock] for
// the two rules this pane enforces on them.
func (p *OverviewPane) WithSummaries(f func() []tui.SummaryBlock) *OverviewPane {
	p.summaries = f
	return p
}

// WithWidthFraction sets the divisor used to size this pane on the fixed top
// row, so several summary panes can share it.
func (p *OverviewPane) WithWidthFraction(n int) *OverviewPane {
	p.widthFraction = n
	return p
}

// FixedWidth implements tui.FixedSizePane.
func (p *OverviewPane) FixedWidth(termWidth int) int {
	if p.widthFraction <= 1 {
		return 0
	}
	return termWidth / p.widthFraction
}

// FixedHeight implements tui.FixedHeightPane: the pane asks for exactly the rows
// its content needs, so no vertical space is wasted above a short summary.
func (p *OverviewPane) FixedHeight(bodyWidth int) int {
	return lineCount(p.body(bodyWidth)) + table.FixedPaneVPad
}

// Render implements tui.Pane.
//
// The body is clipped because this pane composes its own lines rather than
// delegating to the table renderer, and the layout's rectangle is a hard bound.
func (p *OverviewPane) Render(w, h int, _ bool) string {
	return clipTo(p.body(w), w, h)
}

func (p *OverviewPane) body(w int) string {
	st := rollout.Detect(p.store, p.targetVersion)

	var blocks []string
	if b := p.clusterBlock(st); b != "" {
		blocks = append(blocks, b)
	}
	if b := p.poolBlock(w, st); b != "" {
		blocks = append(blocks, b)
	}
	if b := p.nodeBlock(); b != "" {
		blocks = append(blocks, b)
	}
	if b := p.workloadBlock(); b != "" {
		blocks = append(blocks, b)
	}
	blocks = append(blocks, p.contributed(blocks, w)...)

	if len(blocks) == 0 {
		return table.Placeholder(w, 3, "waiting for cluster data…")
	}
	return arrangeBlocks(blocks, w)
}

// contributed renders the plugin blocks that this row can afford, enforcing both
// of [tui.SummaryBlock]'s rules.
//
// A block is taken only while a column remains for it, and trimmed to the tallest
// core block. Both guard the same thing: this pane is a fixed-height row above the
// whole grid, so a block that stacked under another or ran a line longer would
// push every other pane down — and it would do it exactly when a subsystem started
// misbehaving, which is the worst moment for the layout to move.
func (p *OverviewPane) contributed(core []string, w int) []string {
	if p.summaries == nil {
		return nil
	}
	room := max(w/blockColumnWidth, 1) - len(core)
	if room <= 0 {
		return nil
	}

	tallest := 0
	for _, b := range core {
		tallest = max(tallest, lineCount(b))
	}

	var out []string
	for _, block := range p.summaries() {
		if len(out) == room {
			break
		}
		lines := block.Lines
		// One row of the budget is the title.
		if body := tallest - 1; body >= 0 && len(lines) > body {
			lines = lines[:body]
		}
		if rendered := section(block.Title, lines); rendered != "" {
			out = append(out, rendered)
		}
	}
	return out
}

// blockColumnWidth is the width one column of summary blocks needs before a
// second column is worth opening.
const blockColumnWidth = 52

// arrangeBlocks stacks summary blocks vertically, or side by side when the pane
// is wide enough for it.
//
// This pane's content is naturally narrow — a handful of label-and-value lines —
// so on a wide terminal a single column leaves most of the row empty while making
// the pane needlessly tall. Columns use the height that would otherwise be
// wasted horizontally.
func arrangeBlocks(blocks []string, w int) string {
	cols := min(max(w/blockColumnWidth, 1), len(blocks))
	if cols < 2 {
		return strings.Join(blocks, "\n\n")
	}

	// Distribute blocks round-robin so columns stay close to equal height rather
	// than filling the first one completely.
	buckets := make([][]string, cols)
	for i, b := range blocks {
		buckets[i%cols] = append(buckets[i%cols], b)
	}

	colWidth := w / cols
	rendered := make([]string, 0, cols)
	for _, bucket := range buckets {
		body := strings.Join(bucket, "\n\n")
		rendered = append(rendered, lipgloss.NewStyle().Width(colWidth).Render(clipTo(body, colWidth, 1<<16)))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

// clusterBlock names the clusters and their rollout state.
func (p *OverviewPane) clusterBlock(st rollout.State) string {
	snap, ok := store.Get[model.Snapshot[model.Cluster]](p.store, model.KeyMgmtClusters)
	if !ok || snap.Err != nil {
		// No Cluster API on this cluster is a legitimate configuration, not an
		// error to shout about — the node and workload blocks below still
		// carry the pane.
		if st.Active {
			return keyValue(10, "Rollout", tui.StyleWarn.Render("in progress"))
		}
		return ""
	}
	if len(snap.Items) == 0 {
		return tui.StyleMuted.Render("No Cluster API clusters found")
	}

	lines := make([]string, 0, len(snap.Items)+1)
	for _, c := range snap.Items {
		name := c.Namespace + "/" + c.Name
		bits := []string{tui.StatusStyle(c.Phase).Render(orDash(c.Phase))}
		if c.Version != "" {
			bits = append(bits, c.Version)
		}
		if c.Paused {
			bits = append(bits, tui.StyleWarn.Render("paused"))
		}
		if !c.Available && c.Phase != "" {
			bits = append(bits, tui.StyleErr.Render("unavailable"))
		}
		lines = append(lines, keyValue(clusterLabelWidth(snap.Items), name, strings.Join(bits, "  ")))
	}

	switch {
	case st.Active && st.Asserted:
		lines = append(lines, keyValue(clusterLabelWidth(snap.Items), "rollout",
			tui.StyleWarn.Render("target "+st.TargetVersion)))
	case st.Active:
		lines = append(lines, keyValue(clusterLabelWidth(snap.Items), "rollout",
			tui.StyleWarn.Render(fmt.Sprintf("%d pool(s) updating", len(st.Rolling)))))
	}
	return section("Clusters", lines)
}

func clusterLabelWidth(cs []model.Cluster) int {
	w := len("rollout")
	for _, c := range cs {
		w = max(w, len(c.Namespace)+1+len(c.Name))
	}
	return min(w, 40)
}

// poolBlock shows per-pool progress during a rollout and ready counts at rest.
func (p *OverviewPane) poolBlock(w int, st rollout.State) string {
	kcps, kOK := store.Get[model.Snapshot[model.KubeadmControlPlane]](p.store, model.KeyMgmtKCPs)
	mds, mOK := store.Get[model.Snapshot[model.MachineDeployment]](p.store, model.KeyMgmtMachineDeployments)
	if (!kOK || kcps.Err != nil) && (!mOK || mds.Err != nil) {
		return ""
	}

	type pool struct {
		name                     string
		version                  string
		desired, upToDate, ready int32
		paused                   bool
	}
	var pools []pool
	for _, k := range kcps.Items {
		pools = append(pools, pool{
			name: p.roles.DisplayName("control-plane"), version: k.Version,
			desired: k.DesiredReplicas, upToDate: k.UpToDateReplicas, ready: k.ReadyReplicas,
			paused: k.Paused,
		})
	}
	for _, m := range mds.Items {
		name := p.roles.RoleFromMachineDeploymentName(m.Name)
		if name == "" {
			name = m.Name
		} else {
			name = p.roles.DisplayName(name)
		}
		pools = append(pools, pool{
			name: name, version: m.Version,
			desired: m.DesiredReplicas, upToDate: m.UpToDateReplicas, ready: m.ReadyReplicas,
			paused: m.Paused,
		})
	}
	if len(pools) == 0 {
		return ""
	}

	labelW := 0
	for _, pl := range pools {
		labelW = max(labelW, len(pl.name))
	}
	labelW = min(labelW, 24)

	// The bar only earns its space on a wide enough pane; below that the counts
	// alone are more informative than three cells of block characters.
	barW := 0
	if st.Active && w >= labelW+34 {
		barW = min(18, w-labelW-26)
	}

	lines := make([]string, 0, len(pools))
	for _, pl := range pools {
		var right string
		if st.Active {
			right = countStyle(pl.upToDate, pl.desired).Render(
				fmt.Sprintf("%d/%d updated", pl.upToDate, pl.desired))
			if barW > 0 {
				right = progressBar(barW, pl.upToDate, pl.desired) + " " + right
			}
		} else {
			right = countStyle(pl.ready, pl.desired).Render(
				fmt.Sprintf("%d/%d ready", pl.ready, pl.desired))
		}
		if pl.version != "" {
			right += tui.StyleMuted.Render("  " + pl.version)
		}
		if pl.paused {
			right += "  " + tui.StyleWarn.Render("paused")
		}
		lines = append(lines, keyValue(labelW, pl.name, right))
	}

	title := "Node Pools"
	if st.Active {
		title = "Rollout Progress"
	}
	return section(title, lines)
}

// nodeBlock summarizes the workload cluster's nodes by role, plus any pressure
// conditions.
func (p *OverviewPane) nodeBlock() string {
	snap, ok := store.Get[model.Snapshot[model.Node]](p.store, model.KeyWorkloadNodes)
	if !ok || snap.Err != nil || len(snap.Items) == 0 {
		return ""
	}

	type tally struct{ total, ready, cordoned int }
	byRole := map[string]*tally{}
	var order []string
	var pressure []string
	var mem, disk, pid, net int

	for _, n := range snap.Items {
		role := n.Role
		if role == "" {
			role = "(unlabeled)"
		}
		t, seen := byRole[role]
		if !seen {
			t = &tally{}
			byRole[role] = t
			order = append(order, role)
		}
		t.total++
		if n.Ready() {
			t.ready++
		}
		// A role cordoned by design contributes no amber; see
		// profile.NodeRoles.CordonExpected.
		if n.Cordoned && p.roles.CordonIsNews(n.Role, n.Ready()) {
			t.cordoned++
		}
		if n.MemoryPressure {
			mem++
		}
		if n.DiskPressure {
			disk++
		}
		if n.PIDPressure {
			pid++
		}
		if n.NetworkUnavail {
			net++
		}
	}
	sort.Strings(order)

	labelW := 0
	for _, r := range order {
		labelW = max(labelW, len(p.roles.DisplayName(r)))
	}

	lines := make([]string, 0, len(order)+1)
	for _, r := range order {
		t := byRole[r]
		val := countStyle(int32(t.ready), int32(t.total)).Render(
			fmt.Sprintf("%d/%d ready", t.ready, t.total))
		if t.cordoned > 0 {
			val += "  " + tui.StyleWarn.Render(fmt.Sprintf("%d cordoned", t.cordoned))
		}
		lines = append(lines, keyValue(labelW, p.roles.DisplayName(r), val))
	}

	// Only name a pressure that is actually present. A permanent row of four
	// zeroes trains the eye to skip the line entirely.
	for _, pc := range []struct {
		label string
		count int
	}{{"memory", mem}, {"disk", disk}, {"pid", pid}, {"network", net}} {
		if pc.count > 0 {
			pressure = append(pressure, tui.StyleErr.Render(fmt.Sprintf("%s×%d", pc.label, pc.count)))
		}
	}
	if len(pressure) > 0 {
		lines = append(lines, keyValue(labelW, "pressure", strings.Join(pressure, " ")))
	}
	return section("Nodes", lines)
}

// workloadBlock counts Deployments, StatefulSets and DaemonSets that are not
// fully ready.
func (p *OverviewPane) workloadBlock() string {
	snap, ok := store.Get[model.Snapshot[model.Workload]](p.store, model.KeyWorkloadWorkloads)
	if !ok || snap.Err != nil || len(snap.Items) == 0 {
		return ""
	}

	type tally struct{ total, ready int }
	byKind := map[string]*tally{}
	var order []string
	for _, wl := range snap.Items {
		t, seen := byKind[wl.Kind]
		if !seen {
			t = &tally{}
			byKind[wl.Kind] = t
			order = append(order, wl.Kind)
		}
		t.total++
		if wl.Ready >= wl.Desired {
			t.ready++
		}
	}
	sort.Strings(order)

	labelW := 0
	for _, k := range order {
		labelW = max(labelW, len(k))
	}

	lines := make([]string, 0, len(order))
	for _, k := range order {
		t := byKind[k]
		lines = append(lines, keyValue(labelW, k, countStyle(int32(t.ready), int32(t.total)).Render(
			fmt.Sprintf("%d/%d ready", t.ready, t.total))))
	}
	return section("Workloads", lines)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
