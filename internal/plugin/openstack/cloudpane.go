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

// cloudPane reports server migrations.
//
// This was once a mode-aware slot that showed migrations during a rollout and the
// resource inventory the rest of the time, on the reasoning that the two answer
// the same question at different moments. That reasoning held for one reader and
// not for the other. Migrations are driven by whoever is draining a hypervisor,
// and the two people who do that are not synchronized: a Kubernetes upgrade drains
// nodes on the fleet's schedule, while the cloud's own operator drains one host at
// a time to restart the switching layer under it — work that involves no Cluster
// API rollout at all. Keying the mode on the rollout signal therefore hid live
// migrations from precisely the person causing them.
//
// So the inventory moved into the Cloud frame as a section of its own, and this is
// migrations alone. The pane costs one grid column rather than two, which is what
// it uses: the widest row is a status, a type, a short UUID and a pair of compute
// hostnames.
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

// rolling reports whether a Cluster API rollout is under way.
//
// Retained for the empty-state wording only: "no active migrations" is
// reassurance during a rollout, where a drain that had stalled would show here,
// and merely a statement of fact outside one.
func (p *cloudPane) rolling() bool {
	return rollout.Active(p.store, p.targetVersion)
}

// Render implements tui.Pane.
func (p *cloudPane) Render(w, h int, _ bool) string {
	return p.renderMigrations(w, h, time.Now())
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

// Group puts this pane in the shared "Cloud" frame; see [tui.GroupedPane].
func (p *resourcesPane) GroupID() string    { return "cloud" }
func (p *resourcesPane) GroupTitle() string { return "Cloud" }
func (p *resourcesPane) GroupOrder() int    { return 2 }

// Render implements tui.Pane.
func (p *resourcesPane) Render(w, h int, _ bool) string {
	return renderInventory(p.store, w, h)
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

	labelW := 0
	for _, c := range inv.Counts {
		labelW = max(labelW, len(c.Label)+1)
	}

	lines := make([]string, 0, len(inv.Counts))
	for _, c := range inv.Counts {
		lines = append(lines, table.PadOrTrunc(countLine(c, labelW, w), w))
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
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
		// The breakdown is fitted to what is left of the line rather than
		// written in full and clipped. Clipping cuts mid-word — a real cloud
		// rendered "ERROR_DELETING 10, ATTACHI" at the pane edge, which reads as
		// a state that does not exist and hides however many followed it.
		line += "  " + tui.StyleMuted.Render(breakdown(c.ByState, max(width-lipgloss.Width(line)-2, 0)))
	}
	return line
}

// breakdown formats a state map as "(STATE n, STATE n)", most common first and
// ties broken alphabetically, so the eye lands on the bulk state without reading
// the whole parenthesis.
//
// States that do not fit in width are dropped and counted rather than truncated.
// Dropping the rarest is the right sacrifice: they are last precisely because
// they describe the fewest resources, and "+2" is honest about their existence
// where a severed word is not. A width of zero or less means unconstrained, for
// callers that have already made room.
func breakdown(by map[string]int, width int) string {
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
	// Two for the parentheses, and the running total tracks the ", " joins.
	used := 2
	for i, e := range entries {
		part := fmt.Sprintf("%s %d", e.state, e.n)
		cost := len(part)
		if i > 0 {
			cost += 2
		}
		if width > 0 && used+cost > width {
			// Room for the marker is made by dropping one more entry if need be,
			// so the marker itself cannot be what overflows.
			marker := fmt.Sprintf(", +%d", len(entries)-i)
			for len(parts) > 0 && used+len(marker) > width {
				last := parts[len(parts)-1]
				parts = parts[:len(parts)-1]
				used -= len(last) + 2
				marker = fmt.Sprintf(", +%d", len(entries)-len(parts))
			}
			if len(parts) == 0 {
				return ""
			}
			return "(" + strings.Join(parts, ", ") + marker + ")"
		}
		used += cost
		parts = append(parts, part)
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
