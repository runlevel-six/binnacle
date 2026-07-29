package openstack

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/core/rollout"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// cloudPane is the mode-aware slot: server migrations while a rollout is under
// way, cloud inventory the rest of the time.
//
// One slot rather than two panes, because the two contents answer the same
// question at different times. During a rollout every compute node drains and
// every VM moves, so "what is moving, and what failed to move" is the only thing
// worth the space — a stalled migration is what stalls the upgrade. At rest
// nothing is moving and the useful question becomes "what is in this cloud".
// Showing both at once would cost a pane to a table that reads "no active
// migrations" for weeks at a time.
type cloudPane struct {
	store         *store.Store
	targetVersion string
}

func newCloudPane(s *store.Store, targetVersion string) *cloudPane {
	return &cloudPane{store: s, targetVersion: targetVersion}
}

func (p *cloudPane) ID() string { return "openstack-cloud" }

// Title names whichever content the mode has selected, so the border always
// describes what is underneath it.
func (p *cloudPane) Title() string {
	if p.rolling() {
		return "Server Migrations"
	}
	return "OpenStack Resources"
}

func (p *cloudPane) Priority() tui.Priority { return tui.P2Useful }
func (p *cloudPane) MinWidth() int          { return 50 }
func (p *cloudPane) MinHeight() int         { return 5 }
func (p *cloudPane) HeightWeight() int      { return 2 }

// rolling reports whether the rollout-flavored content is the one to show.
//
// The signal is the same one the core panes use, so the whole dashboard changes
// mode together rather than one pane disagreeing with the header.
func (p *cloudPane) rolling() bool {
	return rollout.Active(p.store, p.targetVersion)
}

// Render implements tui.Pane.
func (p *cloudPane) Render(w, h int, _ bool) string {
	if p.rolling() {
		return p.renderMigrations(w, h, time.Now())
	}
	return p.renderInventory(w, h)
}

var migrationCols = []table.Column{
	{Header: "STATUS"},
	{Header: "TYPE"},
	{Header: "SERVER"},
	{Header: "SRC → DST", Stretch: true},
	{Header: "AGE"},
}

func (p *cloudPane) renderMigrations(w, h int, now time.Time) string {
	snap, ok := store.Get[Migrations](p.store, KeyMigrations)
	if !ok {
		return table.Placeholder(w, h, "polling migrations…")
	}
	if snap.Err != nil {
		return table.ErrorBody(w, h, snap.Err)
	}

	items := Relevant(LatestPerServer(snap.Items), now)
	if len(items) == 0 {
		// An idle migration list during a rollout is good news, and saying so is
		// more useful than an empty table: it means the drain is not stuck.
		return table.Placeholder(w, h, "no active migrations")
	}

	rows := make([][]string, 0, len(items))
	styles := make([]lipgloss.Style, 0, len(items))
	failed := 0
	for _, m := range items {
		if Failed(m.Status) {
			failed++
		}
		dst := ShortHost(m.DestCompute)
		if dst == "" {
			// Nova leaves the destination empty until the scheduler has picked
			// one, which is a normal early state rather than missing data.
			dst = "?"
		}
		rows = append(rows, []string{
			ShortStatus(m.Status),
			ShortType(m.Type),
			shortUUID(m.InstanceUUID),
			ShortHost(m.SourceCompute) + " → " + dst,
			age(now, m.UpdatedAt),
		})
		styles = append(styles, migrationStyle(m.Status))
	}

	summary := tui.StyleAccent.Render(fmt.Sprintf("%d active", len(items)-failed))
	if failed > 0 {
		summary += "  " + tui.StyleErr.Render(fmt.Sprintf("%d failed", failed))
	}

	t := table.Table{Cols: migrationCols, Rows: rows, RowStyles: styles}
	return table.ClipLines(summary+"\n"+t.Render(w, h-1), h)
}

// renderInventory is the at-rest content: cloud-wide counts.
//
// Every row renders independently of the others' success, so a denied Keystone or
// an absent Octavia costs one line rather than the pane.
func (p *cloudPane) renderInventory(w, h int) string {
	inv, ok := store.Get[Inventory](p.store, KeyInventory)
	if !ok {
		return table.Placeholder(w, h, "polling OpenStack resources…")
	}
	if inv.Err != nil {
		return table.ErrorBody(w, h, inv.Err)
	}
	if len(inv.Counts) == 0 {
		return table.Placeholder(w, h, "no resources counted")
	}

	labelW := 0
	for _, c := range inv.Counts {
		labelW = max(labelW, len(c.Label)+1)
	}

	lines := make([]string, 0, len(inv.Counts))
	for _, c := range inv.Counts {
		lines = append(lines, table.PadOrTrunc(countLine(c, labelW), w))
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
}

func countLine(c Count, labelW int) string {
	label := fmt.Sprintf("%-*s", labelW, c.Label+":")
	switch {
	case c.Absent:
		return tui.StyleMuted.Render(label + " not deployed")
	case c.Err != nil:
		return tui.StyleMuted.Render(label+" ") + tui.StyleWarn.Render(trimErr(c.Err.Error()))
	}

	line := label + fmt.Sprintf(" %d", c.Total)
	if len(c.ByState) > 0 {
		line += "  " + tui.StyleMuted.Render(breakdown(c.ByState))
	}
	return line
}

// breakdown formats a state map as "(STATE n, STATE n)", most common first and
// ties broken alphabetically, so the eye lands on the bulk state without reading
// the whole parenthesis.
func breakdown(by map[string]int) string {
	type entry struct {
		state string
		n     int
	}
	entries := make([]entry, 0, len(by))
	for state, n := range by {
		entries = append(entries, entry{state, n})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].n != entries[j].n {
			return entries[i].n > entries[j].n
		}
		return entries[i].state < entries[j].state
	})

	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s %d", e.state, e.n))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func migrationStyle(status string) lipgloss.Style {
	var plain lipgloss.Style
	switch strings.ToLower(status) {
	case "failed", "error":
		return tui.StyleErr
	case "queued", "preparing", "accepted":
		// Waiting to start. Amber, because a queue that never drains is the
		// second-most-common way a drain stalls.
		return tui.StyleWarn
	case "running", "migrating", "pre-migrating", "post-migrating":
		return tui.StyleAccent
	}
	return plain
}

// shortUUID keeps the first eight hex characters — enough to grep against
// `openstack server list` without letting one column own the row.
func shortUUID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// age formats how long ago a migration was last updated. An unparseable or
// missing timestamp reads as unknown rather than as an implausible age.
func age(now, at time.Time) string {
	if at.IsZero() {
		return "?"
	}
	return table.ShortAge(now.Sub(at).Seconds())
}

// trimErr keeps a message short enough to sit on one line beside its label. The
// full error stays available in --debug-snapshot.
func trimErr(s string) string {
	const limit = 48
	if len(s) > limit {
		return s[:limit-1] + "…"
	}
	return s
}
