package pane

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/pkg/rollout"
	"github.com/runlevel-six/binnacle/pkg/store"
	osstate "github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

// cloudPane reports server migrations.
type cloudPane struct {
	store         *store.Store
	targetVersion string
}

func newCloudPane(s *store.Store, targetVersion string) *cloudPane {
	return &cloudPane{store: s, targetVersion: targetVersion}
}

func (p *cloudPane) ID() string    { return "openstack-cloud" }
func (p *cloudPane) Title() string { return "Server Migrations" }

func (p *cloudPane) Priority() tui.Priority { return tui.P2Useful }
func (p *cloudPane) MinWidth() int          { return 50 }
func (p *cloudPane) MinHeight() int         { return 5 }
func (p *cloudPane) HeightWeight() int      { return 2 }

func (p *cloudPane) rolling() bool {
	return rollout.Active(p.store, p.targetVersion)
}

func (p *cloudPane) Render(w, h int, _ bool) string {
	return p.renderMigrations(w, h, time.Now())
}

func (p *cloudPane) ContentWidth() int {
	rows, _ := p.migrationRows(time.Now())
	want := 0
	if len(rows) > 0 {
		want = table.AppetiteWidth(migrationCols, rows)
	}
	if hosts := p.drainHosts(); len(hosts) > 0 {
		want = max(want, tui.WidestLine(hosts)+table.EdgePadCap)
	}
	return want
}

func (p *cloudPane) ContentHeight(int) int {
	rows, ok := p.migrationRows(time.Now())
	drains := len(p.drainHosts())
	if drains > 0 {
		drains += drainSectionCost
	}
	switch {
	case !ok:
		return 0
	case len(rows) == 0 && drains == 0:
		return 1
	case len(rows) == 0:
		return drains + 1
	}
	return drains + len(rows) + 2
}

func (p *cloudPane) drainHosts() []string {
	snap, ok := store.Get[Migrations](p.store, KeyMigrations)
	if !ok || snap.Err != nil {
		return nil
	}
	return drainHostLines(snap)
}

// resourcesPane is the cloud's resource census, as a section of the Cloud frame.
type resourcesPane struct {
	store *store.Store
}

func newResourcesPane(s *store.Store) *resourcesPane { return &resourcesPane{store: s} }

func (p *resourcesPane) ID() string             { return "openstack-resources" }
func (p *resourcesPane) Title() string          { return "Resources" }
func (p *resourcesPane) Priority() tui.Priority { return tui.P2Useful }
func (p *resourcesPane) MinWidth() int          { return 40 }
func (p *resourcesPane) MinHeight() int         { return 4 }
func (p *resourcesPane) HeightWeight() int      { return 2 }

func (p *resourcesPane) GroupID() string    { return "cloud" }
func (p *resourcesPane) GroupTitle() string { return "Cloud" }
func (p *resourcesPane) GroupOrder() int    { return 2 }

func (p *resourcesPane) Render(w, h int, _ bool) string {
	return renderInventory(p.store, w, h)
}

func (p *resourcesPane) ContentWidth() int {
	lines, ok := inventoryLines(p.store, 0)
	if !ok {
		return 0
	}
	return tui.WidestLine(lines)
}

func (p *resourcesPane) ContentHeight(int) int {
	lines, ok := inventoryLines(p.store, 0)
	if !ok {
		return 0
	}
	return len(lines)
}

var migrationCols = []table.Column{
	{Header: "STATUS"},
	{Header: "TYPE"},
	{Header: "SERVER"},
	{Header: "SRC → DST", Stretch: true},
	{Header: "AGE"},
}

var unresolvedCols = []table.Column{
	{Header: "SERVER"},
	{Header: "ON HOST"},
	{Header: "AGE"},
	{Header: "FAULT", Stretch: true},
}

func drainHostLines(snap Migrations) []string {
	if len(snap.Drains) == 0 {
		return nil
	}

	lines := make([]string, 0, len(snap.Drains)+1)
	for _, d := range snap.Drains {
		host := tui.StyleAccent.Render(ShortHost(d.Host))
		switch {
		case d.Err != nil:
			lines = append(lines, host+": "+tui.StyleWarn.Render("server count unavailable"))
			continue
		case d.Remaining == 0:
			lines = append(lines, host+": "+tui.StyleOK.Render("empty"))
			continue
		}

		parts := []string{fmt.Sprintf("%d left", d.Remaining)}
		if d.Moving > 0 {
			parts = append(parts, fmt.Sprintf("%d moving", d.Moving))
		} else {
			parts = append(parts, tui.StyleWarn.Render("none moving"))
		}
		if d.Stuck > 0 {
			parts = append(parts, tui.StyleErr.Render(fmt.Sprintf("%d stuck", d.Stuck)))
		}
		lines = append(lines, host+": "+strings.Join(parts, ", "))
	}

	if rest := len(snap.Draining) - len(snap.Drains); rest > 0 {
		lines = append(lines, tui.StyleMuted.Render(fmt.Sprintf("+ %d more draining", rest)))
	}
	return lines
}

const drainSectionCost = 2

func drainLines(snap Migrations, width int, headed bool) []string {
	body := drainHostLines(snap)
	if len(body) == 0 {
		return nil
	}

	if !headed {
		for i, ln := range body {
			body[i] = "draining " + ln
		}
		return append(body, "")
	}

	indent := strings.Repeat(" ", table.PaneLeftPad(width, migrationCols))
	out := make([]string, 0, len(body)+drainSectionCost)
	out = append(out, tui.StyleAccent.Render("Draining"))
	for _, ln := range body {
		out = append(out, indent+ln)
	}
	return append(out, "")
}

func (p *cloudPane) migrationRows(now time.Time) (rows [][]string, ok bool) {
	snap, have := store.Get[Migrations](p.store, KeyMigrations)
	if !have || snap.Err != nil {
		return nil, false
	}
	rows, _ = migrationTable(snap, snap.Relevant(now).Rows, now)
	return rows, true
}

func migrationTable(snap Migrations, items []Migration, now time.Time) ([][]string, []lipgloss.Style) {
	rows := make([][]string, 0, len(items))
	styles := make([]lipgloss.Style, 0, len(items))
	for _, m := range items {
		dst := ShortHost(m.DestCompute)
		if dst == "" {
			dst = "?"
		}
		rows = append(rows, []string{
			ShortStatus(m.Status),
			ShortType(m.Type),
			shortUUID(m.InstanceUUID),
			ShortHost(m.SourceCompute) + " → " + dst,
			age(now, m.UpdatedAt),
		})
		styles = append(styles, migrationStyle(m.Status, snap.StillBroken(m)))
	}
	return rows, styles
}

func unresolvedRows(snap Migrations, items []Migration, now time.Time) [][]string {
	rows := make([][]string, 0, len(items))
	for _, m := range items {
		b := snap.Broken[m.InstanceUUID]
		fault := b.Fault
		if fault == "" {
			fault = "—"
		}
		rows = append(rows, []string{
			shortUUID(m.InstanceUUID),
			ShortHost(b.Host),
			age(now, m.UpdatedAt),
			fault,
		})
	}
	return rows
}

func failureBudget(failures, available int) int {
	if available <= 0 {
		return 0
	}
	if failures <= available/2 {
		return failures
	}
	return max(available/2, 1)
}

func (p *cloudPane) renderMigrations(w, h int, now time.Time) string {
	snap, ok := store.Get[Migrations](p.store, KeyMigrations)
	if !ok {
		return table.Placeholder(w, h, "polling migrations…")
	}
	if snap.Err != nil {
		return table.ErrorBody(w, h, snap.Err)
	}

	shown := snap.Relevant(now)
	hosts := len(drainHostLines(snap))
	below := 1
	if len(shown.Rows) > 0 {
		below = 3
	}
	drains := drainLines(snap, w, hosts > 0 && h >= hosts+drainSectionCost+below)
	if len(shown.Rows) == 0 && len(shown.Unresolved) == 0 && len(drains) == 0 {
		if p.rolling() {
			return table.Placeholder(w, h, "no active migrations")
		}
		return table.Placeholder(w, h, "no migrations")
	}

	sep := below
	if seats := h - len(drains) - 2; len(shown.Rows) > seats {
		sep++
	}
	if len(drains) > 0 && h-len(drains) < sep && drains[len(drains)-1] == "" {
		drains = drains[:len(drains)-1]
	}

	lead := ""
	if len(drains) > 0 {
		lead = strings.Join(drains, "\n") + "\n"
	}
	mh := max(h-len(drains), 0)
	if len(shown.Rows) == 0 && len(shown.Unresolved) == 0 {
		return table.ClipLines(lead+tui.StyleMuted.Render("no migrations in flight"), h)
	}

	failed := shown.Failures()
	summary := tui.StyleAccent.Render(fmt.Sprintf("%d active", len(shown.Rows)-failed))
	if failed > 0 {
		summary += "  " + tui.StyleErr.Render(fmt.Sprintf("%d failed", failed))
	}
	if n := len(shown.Unresolved); n > 0 {
		summary += "  " + tui.StyleWarn.Render(fmt.Sprintf("%d unresolved", n))
	}

	body := max(mh-2, 0)
	listed := shown.Rows
	if keep := failureBudget(failed, body); keep < failed {
		listed = append(append([]Migration{}, shown.Rows[:keep]...), shown.Rows[failed:]...)
	}

	rows, styles := migrationTable(snap, listed, now)
	main := table.Table{Cols: migrationCols, Rows: rows, RowStyles: styles}
	out := lead + summary + "\n" + main.Render(w, mh-1)

	if spare := mh - 1 - len(rows) - 1; spare >= 3 && len(shown.Unresolved) > 0 {
		detail := table.Table{
			Cols: unresolvedCols,
			Rows: unresolvedRows(snap, shown.Unresolved, now),
		}
		heading := tui.StyleWarn.Render(fmt.Sprintf("Unresolved failures (%d)", len(shown.Unresolved)))
		out += "\n\n" + heading + "\n" + detail.Render(w, spare-2)
	}

	return table.ClipLines(out, h)
}

func renderInventory(s *store.Store, w, h int) string {
	inv, ok := store.Get[Inventory](s, KeyInventory)
	if !ok {
		return table.Placeholder(w, h, "polling OpenStack resources…")
	}
	if inv.Err != nil {
		return table.ErrorBody(w, h, inv.Err)
	}
	if len(inv.Counts) == 0 {
		return table.Placeholder(w, h, "no resources counted")
	}

	lines, _ := inventoryLines(s, w)
	for i, ln := range lines {
		lines[i] = table.PadOrTrunc(ln, w)
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
}

func inventoryLines(s *store.Store, width int) (lines []string, ok bool) {
	inv, have := store.Get[Inventory](s, KeyInventory)
	if !have || inv.Err != nil || len(inv.Counts) == 0 {
		return nil, false
	}

	labelW := 0
	for _, c := range inv.Counts {
		labelW = max(labelW, len(c.Label)+1)
	}
	lines = make([]string, 0, len(inv.Counts))
	for _, c := range inv.Counts {
		lines = append(lines, countLine(c, labelW, width))
	}
	return lines, true
}

func countLine(c Count, labelW, width int) string {
	label := fmt.Sprintf("%-*s", labelW, c.Label+":")
	switch {
	case c.Absent:
		return tui.StyleMuted.Render(label + " not deployed")
	case c.Err != nil:
		return tui.StyleMuted.Render(label+" ") + tui.StyleWarn.Render(trimErr(c.Err.Error()))
	}

	line := label + fmt.Sprintf(" %d", c.Total)
	if len(c.ByState) > 0 {
		line += "  " + breakdown(c.ByState, max(width-lipgloss.Width(line)-2, 0))
	}
	return line
}

func breakdown(by map[string]int, width int) string {
	entries := osstate.StateCounts(by)

	plain := make([]string, 0, len(entries))
	styled := make([]string, 0, len(entries))
	join := tui.StyleMuted.Render(", ")
	wrap := func(parts []string, tail string) string {
		return tui.StyleMuted.Render("(") + strings.Join(parts, join) +
			tui.StyleMuted.Render(tail+")")
	}

	used := 2
	for i, e := range entries {
		part := fmt.Sprintf("%s %d", e.State, e.Count)
		cost := len(part)
		if i > 0 {
			cost += 2
		}
		if width > 0 && used+cost > width {
			marker := fmt.Sprintf(", +%d", len(entries)-i)
			for len(plain) > 0 && used+len(marker) > width {
				last := plain[len(plain)-1]
				plain = plain[:len(plain)-1]
				styled = styled[:len(styled)-1]
				used -= len(last) + 2
				marker = fmt.Sprintf(", +%d", len(entries)-len(plain))
			}
			if len(plain) == 0 {
				return ""
			}
			return wrap(styled, marker)
		}
		used += cost
		plain = append(plain, part)
		if e.Error {
			styled = append(styled, tui.StyleErr.Render(part))
		} else {
			styled = append(styled, tui.StyleMuted.Render(part))
		}
	}
	return wrap(styled, "")
}

func migrationStyle(status string, broken bool) lipgloss.Style {
	var plain lipgloss.Style
	switch strings.ToLower(status) {
	case "failed", "error":
		if broken {
			return tui.StyleErr
		}
		return tui.StyleWarn
	case "queued", "preparing", "accepted":
		return tui.StyleWarn
	case "running", "migrating", "pre-migrating", "post-migrating":
		return tui.StyleAccent
	}
	return plain
}

func shortUUID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func age(now, at time.Time) string {
	if at.IsZero() {
		return "?"
	}
	return table.ShortAge(now.Sub(at).Seconds())
}

func trimErr(s string) string {
	const limit = 48
	if len(s) > limit {
		return s[:limit-1] + "…"
	}
	return s
}
