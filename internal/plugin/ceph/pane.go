package ceph

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// pane renders Ceph's health and capacity.
type pane struct {
	store *store.Store
}

func newPane(s *store.Store) *pane { return &pane{store: s} }

func (p *pane) ID() string             { return "ceph" }
func (p *pane) Title() string          { return "Ceph" }
func (p *pane) Priority() tui.Priority { return tui.P1Important }
func (p *pane) MinWidth() int          { return 44 }
func (p *pane) MinHeight() int         { return 7 }
func (p *pane) HeightWeight() int      { return 2 }

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading Ceph…")
	}
	if state.Err != nil {
		return table.ErrorBody(w, h, state.Err)
	}
	if state.Tier != kube.TierFull {
		return table.Placeholder(w, h, "Ceph detail unavailable — "+state.TierReason)
	}

	st := state.Status
	labelW := 10
	lines := []string{
		row(labelW, "health", healthText(st)),
		row(labelW, "mons", monText(st.Mons)),
		row(labelW, "mgr", mgrText(st.Mgr)),
		row(labelW, "osds", osdText(st.OSDs)),
		row(labelW, "pgs", pgText(st.PGs)),
		row(labelW, "capacity", capacityText(st.PGs)),
		row(labelW, "client io", ioText(st.IO)),
	}
	if len(st.Unreadable) > 0 {
		lines = append(lines, row(labelW, "unreadable",
			tui.StyleWarn.Render(strings.Join(st.Unreadable, ", "))))
	}
	// Failing checks are named, since "HEALTH_WARN" alone gives nothing to act on.
	for _, c := range st.Checks {
		lines = append(lines, "  "+checkStyle(c.Severity).Render(c.Name)+" "+
			tui.StyleMuted.Render(c.Message))
	}
	return clip(strings.Join(lines, "\n"), w, h)
}

func row(labelW int, label, value string) string {
	return tui.StyleMuted.Render(table.PadOrTrunc(label, labelW)) + " " + value
}

func healthText(st Status) string {
	text := st.Health
	if text == "" {
		text = "unknown"
	}
	out := healthStyle(st.Health).Render(text)
	if st.MutedChecks > 0 {
		// A muted check is a suppressed warning, so an OK status with mutes is
		// not the same as an unqualified OK.
		out += "  " + tui.StyleWarn.Render(fmt.Sprintf("%d muted", st.MutedChecks))
	}
	return out
}

func healthStyle(health string) lipgloss.Style {
	switch health {
	case "HEALTH_OK":
		return tui.StyleOK
	case "HEALTH_WARN":
		return tui.StyleWarn
	case "HEALTH_ERR":
		return tui.StyleErr
	}
	return tui.StyleMuted
}

func checkStyle(severity string) lipgloss.Style {
	switch severity {
	case "HEALTH_ERR":
		return tui.StyleErr
	case "HEALTH_WARN":
		return tui.StyleWarn
	}
	return tui.StyleMuted
}

func monText(m Mons) string {
	if m.Total == 0 {
		return tui.StyleMuted.Render("not reported")
	}
	text := fmt.Sprintf("%d/%d in quorum", m.InQuorum, m.Total)
	if m.Healthy() {
		return tui.StyleOK.Render(text)
	}
	// A monitor out of quorum is one step from losing the cluster's control plane.
	return tui.StyleErr.Render(text)
}

func mgrText(m Mgr) string {
	if !m.Available {
		return tui.StyleErr.Render("no active manager")
	}
	name := m.Active
	if m.ActiveUnknown() {
		// The summary-only mgrmap omits the name, and the follow-up query failed.
		// That is a gap in the data, not a missing manager.
		name = tui.StyleMuted.Render("name not reported")
	}
	return fmt.Sprintf("%s %s", tui.StyleOK.Render("active"), name) +
		tui.StyleMuted.Render(fmt.Sprintf("  +%d standby", m.Standbys))
}

func osdText(o OSDs) string {
	if o.Total == 0 {
		return tui.StyleMuted.Render("not reported")
	}
	text := fmt.Sprintf("%d/%d up, %d in", o.Up, o.Total, o.In)
	if o.Healthy() {
		return tui.StyleOK.Render(text)
	}
	return tui.StyleWarn.Render(text)
}

func pgText(p PGs) string {
	if p.Total == 0 {
		return tui.StyleMuted.Render("not reported")
	}
	if p.AllClean() {
		return tui.StyleOK.Render(fmt.Sprintf("%d active+clean", p.Total)) +
			tui.StyleMuted.Render(fmt.Sprintf("  %d pools, %s objects", p.Pools, count(p.Objects)))
	}
	// Name the states that are not clean; that is what says whether the cluster is
	// recovering or stuck.
	var parts []string
	for _, s := range p.ByState {
		if s.Name == "active+clean" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", s.Count, s.Name))
	}
	return tui.StyleWarn.Render(fmt.Sprintf("%d/%d clean", p.CleanPGs(), p.Total)) +
		tui.StyleMuted.Render("  "+strings.Join(parts, ", "))
}

// capacityText reports raw usage, and stored data separately.
//
// Raw usage is what fills the cluster, and it includes replication — a three-way
// pool consumes roughly three times its stored data. Showing only stored data
// would understate a nearly full cluster by that factor.
func capacityText(p PGs) string {
	if p.TotalBytes <= 0 {
		return tui.StyleMuted.Render("not reported")
	}
	pct := p.UsedPercent()
	style := tui.StyleOK
	switch {
	case pct >= 85:
		style = tui.StyleErr
	case pct >= 70:
		style = tui.StyleWarn
	}
	return fmt.Sprintf("%s/%s %s",
		bytes(p.UsedBytes), bytes(p.TotalBytes), style.Render(fmt.Sprintf("%d%%", pct))) +
		tui.StyleMuted.Render(fmt.Sprintf("  %s stored", bytes(p.DataBytes)))
}

func ioText(io IO) string {
	if io.ReadBytesPerSec == 0 && io.WriteBytesPerSec == 0 &&
		io.ReadOpsPerSec == 0 && io.WriteOpsPerSec == 0 {
		return tui.StyleMuted.Render("idle")
	}
	return fmt.Sprintf("%s/s read, %s/s write", bytes(io.ReadBytesPerSec), bytes(io.WriteBytesPerSec)) +
		tui.StyleMuted.Render(fmt.Sprintf("  %d/%d iops", io.ReadOpsPerSec, io.WriteOpsPerSec))
}

// bytes formats a byte count in binary units.
func bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 5 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", value, "KMGTP"[exp-1])
}

// count abbreviates a large object count, which routinely runs to millions.
func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
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

// summaryBlock is Ceph's headline for the overview pane.
//
// This is where Ceph reports, rather than a pane of its own. Storage is a
// prerequisite for the thing this tool actually watches — you do not drain a host
// unless the cluster can lose it — so it belongs beside the cluster and node
// summaries, not in a column at the bottom competing with them. The block is
// present whenever the plugin is active, and a cluster with no Ceph contributes
// nothing at all, which is what keeps the overview CAPI-and-Metal3-first.
//
// Three lines, because the overview trims to the height of its own blocks and a
// contributed one must not lengthen that row. They are dense on purpose: the
// column is around fifty wide, so pairing related figures fits what used to take
// eight rows into the budget. What does not fit — the placement-group breakdown,
// client throughput, the manager's name — is the detail this deliberately trades
// for the space.
func summaryBlock(s *store.Store) (tui.SummaryBlock, bool) {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		// Nothing published yet. A column saying "loading" would claim the space
		// before earning it; the banner already reports a subsystem it cannot read.
		return tui.SummaryBlock{}, false
	}
	if state.Err != nil {
		return tui.SummaryBlock{
			Title: "Ceph",
			Lines: []string{tui.StyleErr.Render(clipLine(state.Err.Error()))},
		}, true
	}
	if state.Tier != kube.TierFull {
		// Present but unreadable in detail. Say which, and why, in one line.
		return tui.SummaryBlock{
			Title: "Ceph",
			Lines: []string{
				row(9, "health", healthStyle(state.Status.Health).Render(orUnknown(state.Status.Health))),
				tui.StyleMuted.Render(clipLine("no detail — " + state.TierReason)),
			},
		}, true
	}

	st := state.Status
	health := healthStyle(st.Health).Render(st.Health)
	// The failing check rides on the health line: it names what is wrong, and it
	// is only present when something is.
	if len(st.Checks) > 0 {
		health += "  " + tui.StyleWarn.Render(st.Checks[0].Name)
	}

	osds := fmt.Sprintf("%d/%d up, %d in", st.OSDs.Up, st.OSDs.Total, st.OSDs.In)
	if st.Mons.Total > 0 {
		osds += fmt.Sprintf("   mons %d/%d", st.Mons.InQuorum, st.Mons.Total)
	}

	capacity := "—"
	if pct := st.PGs.UsedPercent(); pct >= 0 {
		capacity = fmt.Sprintf("%d%% used", pct)
	}
	if st.PGs.Total > 0 {
		capacity += fmt.Sprintf("   pgs %d/%d clean", st.PGs.CleanPGs(), st.PGs.Total)
	}

	return tui.SummaryBlock{Title: "Ceph", Lines: []string{
		row(9, "health", health),
		row(9, "osds", osds),
		row(9, "capacity", capacity),
	}}, true
}

// clipLine keeps a contributed line inside a summary column.
func clipLine(s string) string {
	const max = 46
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
