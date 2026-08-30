package openstack

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/rollout"
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
// Used for the empty-state wording only. The pane itself is no longer
// mode-aware: migrations are shown whoever is draining, and the two people who
// drain hosts are not synchronized.
func (p *cloudPane) rolling() bool {
	return rollout.Active(p.store, p.targetVersion)
}

// Render implements tui.Pane.
func (p *cloudPane) Render(w, h int, _ bool) string {
	return p.renderMigrations(w, h, time.Now())
}

// ContentWidth implements [tui.ContentWidthPane]: the migration table, which is a
// status, a type, a short UUID and a pair of compute hostnames.
//
// The drain block is measured too, and takes the wider of the two. It is a
// short line, so it usually loses; a cloud draining several hosts at once with
// nothing yet in flight is the case where it does not, and letting the table's
// appetite speak for a pane whose only content is the block would ask for a
// width nothing is drawn at.
func (p *cloudPane) ContentWidth() int {
	rows, _ := p.migrationRows(time.Now())
	want := 0
	if len(rows) > 0 {
		want = table.AppetiteWidth(migrationCols, rows)
	}
	if hosts := p.drainHosts(); len(hosts) > 0 {
		// The same allowance AppetiteWidth makes: these lines are indented to
		// the tables' left edge, so a pane given exactly the bare text width
		// would truncate them by the pad.
		want = max(want, tui.WidestLine(hosts)+table.EdgePadCap)
	}
	return want
}

// ContentHeight implements [tui.ContentHeightPane]: the drain block, the
// active-count line, a header, and one row per migration in flight.
//
// An idle cloud declares a single line, which is the case worth being exact
// about: this pane spends most of its life saying "no migrations", and a whole
// grid row spent centering those two words is a row the fleet tables wanted.
//
// The unresolved backlog is deliberately not counted here; see [migrationRows].
func (p *cloudPane) ContentHeight(int) int {
	rows, ok := p.migrationRows(time.Now())
	// The headed form, since that is what Render draws whenever the grid grants
	// what is asked for here. Asking for the compact form's height would be a
	// request for exactly the size at which the heading has to be dropped.
	drains := len(p.drainHosts())
	if drains > 0 {
		drains += drainSectionCost
	}
	switch {
	case !ok:
		// Still polling, or the poll failed. Both draw a body the reader may need
		// to read at whatever size the row happens to be.
		return 0
	case len(rows) == 0 && drains == 0:
		return 1
	case len(rows) == 0:
		// The block, plus the line saying nothing is in flight — which during a
		// stalled drain is the reading that matters.
		return drains + 1
	}
	return drains + len(rows) + 2
}

// drainHosts is the unlabelled per-host progress lines for the current
// snapshot, or nil. The extent methods measure these rather than the composed
// block, so they do not depend on which form Render picks.
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

// Group puts this pane in the shared "Cloud" frame; see [tui.GroupedPane].
func (p *resourcesPane) GroupID() string    { return "cloud" }
func (p *resourcesPane) GroupTitle() string { return "Cloud" }
func (p *resourcesPane) GroupOrder() int    { return 2 }

// Render implements tui.Pane.
func (p *resourcesPane) Render(w, h int, _ bool) string {
	return renderInventory(p.store, w, h)
}

// ContentWidth implements [tui.ContentWidthPane]: the widest census line with its
// state breakdown written out in full.
//
// Measured unconstrained on purpose. The breakdown is fitted to the width it is
// given and drops the rarest states to make room, so measuring it at some assumed
// width would ask for exactly the space needed to keep dropping them.
func (p *resourcesPane) ContentWidth() int {
	lines, ok := inventoryLines(p.store, 0)
	if !ok {
		return 0
	}
	return tui.WidestLine(lines)
}

// ContentHeight implements [tui.ContentHeightPane]: one line per counted
// resource kind, which is a property of the cloud's deployed services rather than
// of anything running on it.
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

// unresolvedCols is the zoomed detail table: which server, where it actually
// is, how long it has been that way, and Nova's own reason.
//
// The fault stretches because it is the only column whose useful length is
// unbounded, and it is last because it is the one worth truncating.
var unresolvedCols = []table.Column{
	{Header: "SERVER"},
	{Header: "ON HOST"},
	{Header: "AGE"},
	{Header: "FAULT", Stretch: true},
}

// drainHostLines is one line per host being emptied, unlabelled and unindented.
//
// An empty host is called out rather than left as "0 left", because it is the
// line the operator is waiting for: the drain is finished and the maintenance
// can start. A stalled one is the same shape with nothing moving, which a
// migration table alone cannot distinguish from a finished one.
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
			// Servers left and none of them moving. Said plainly, because an
			// empty migration table looks exactly like a finished drain.
			parts = append(parts, tui.StyleWarn.Render("none moving"))
		}
		if d.Stuck > 0 {
			parts = append(parts, tui.StyleErr.Render(fmt.Sprintf("%d stuck", d.Stuck)))
		}
		lines = append(lines, host+": "+strings.Join(parts, ", "))
	}

	// Hosts past the probe cap. Named as a count rather than omitted, so the
	// block cannot imply the cloud has fewer drains running than it has.
	if rest := len(snap.Draining) - len(snap.Drains); rest > 0 {
		lines = append(lines, tui.StyleMuted.Render(fmt.Sprintf("+ %d more draining", rest)))
	}
	return lines
}

// drainSectionCost is what the headed form spends beyond its host lines: a
// heading, and the blank line that separates it from the migrations below.
const drainSectionCost = 2

// drainLines is the progress block as it will be drawn.
//
// It leads the pane because it is the frame the table is read inside. A row
// saying a server is moving off some host means one thing when that host is the
// one you are waiting on and another when it is not, and the complaint that
// started this work — migrations showing that had nothing to do with the drain
// — was in large part that the pane never said which host that was.
//
// headed sets the section off the way the unresolved detail below it is set
// off: a heading, the hosts indented under it to the same left edge the tables
// use, and a blank line after. That is two lines, which a pane at [MinHeight]
// does not have — so the compact form folds the label into each line instead
// and spends one, keeping the separator and losing only the heading. The
// heading is the part that can be inferred from the words; the gap is not.
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

	// Aligned with the migration table's own left edge rather than a margin of
	// this pane's invention; see [table.PaneLeftPad].
	indent := strings.Repeat(" ", table.PaneLeftPad(width, migrationCols))
	out := make([]string, 0, len(body)+drainSectionCost)
	out = append(out, tui.StyleAccent.Render("Draining"))
	for _, ln := range body {
		out = append(out, indent+ln)
	}
	return append(out, "")
}

// migrationRows builds the table rows for the migrations worth listing, and
// reports whether the poll has produced a usable answer at all.
//
// This is what the extent methods measure, so it deliberately covers the
// compact form only: the unresolved backlog is not in it. Asking the grid for
// height to draw the backlog would hand this pane a tall cell in the ordinary
// layout, and leave zoom with nothing further to show.
func (p *cloudPane) migrationRows(now time.Time) (rows [][]string, ok bool) {
	snap, have := store.Get[Migrations](p.store, KeyMigrations)
	if !have || snap.Err != nil {
		return nil, false
	}
	rows, _ = migrationTable(snap, snap.Relevant(now).Rows, now)
	return rows, true
}

// migrationTable renders the in-flight table's cells and per-row styles.
func migrationTable(snap Migrations, items []Migration, now time.Time) ([][]string, []lipgloss.Style) {
	rows := make([][]string, 0, len(items))
	styles := make([]lipgloss.Style, 0, len(items))
	for _, m := range items {
		dst := ShortHost(m.DestCompute)
		if dst == "" {
			// Nova leaves the destination empty until the scheduler has picked one,
			// which is a normal early state rather than missing data.
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

// unresolvedRows renders the detail table behind the summary's unresolved count.
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

// failureBudget caps how many failure rows may be drawn, so a backlog of them
// cannot evict the drain the operator is actually watching.
//
// Failures sort first, so without this the table's own clipping keeps every
// failure and drops every active one — the exact inversion of what a pane
// watched during a drain is for. Half the available rows, and never fewer than
// one: a single failure is the thing most worth seeing, and a pane too short to
// show both still shows that.
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
	// Headed when there is room for the heading and the blank line on top of
	// whatever follows: a summary, a table header and at least one row worth
	// reading, or the single line saying nothing is in flight.
	hosts := len(drainHostLines(snap))
	below := 1
	if len(shown.Rows) > 0 {
		below = 3
	}
	drains := drainLines(snap, w, hosts > 0 && h >= hosts+drainSectionCost+below)
	if len(shown.Rows) == 0 && len(shown.Unresolved) == 0 && len(drains) == 0 {
		// An idle list during a rollout is good news — it means the drain is not
		// stuck — so it is worth saying rather than leaving an empty table. Outside
		// one there is no drain to be reassured about, and the same words would
		// imply a process that is not running.
		if p.rolling() {
			return table.Placeholder(w, h, "no active migrations")
		}
		return table.Placeholder(w, h, "no migrations")
	}

	// At the very bottom of the size range the separator is the first thing to
	// go. A table left with only its header and a "+ N more" tells the reader
	// nothing, and the gap is a courtesy where that row is not.
	//
	// One line more than the heading asks for when the rows will not all fit,
	// because a table that has to truncate spends one of them on the overflow
	// marker: three lines below the block would buy a summary, a header and a
	// "+ N more", and no content at all. Rows that fit need no such allowance.
	sep := below
	if seats := h - len(drains) - 2; len(shown.Rows) > seats {
		sep++
	}
	if len(drains) > 0 && h-len(drains) < sep && drains[len(drains)-1] == "" {
		drains = drains[:len(drains)-1]
	}

	// A drain with nothing moving publishes no migration rows at all, and that
	// is exactly when its progress line is the whole point of the pane.
	lead := ""
	if len(drains) > 0 {
		lead = strings.Join(drains, "\n") + "\n"
	}
	// Height left for the migration content once the block has taken its lines.
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
		// Retained but not listed. Named here so a backlog of broken instances
		// is never silently absent, and enumerated on zoom.
		summary += "  " + tui.StyleWarn.Render(fmt.Sprintf("%d unresolved", n))
	}

	// One line for the summary, one for the table's header.
	body := max(mh-2, 0)
	listed := shown.Rows
	if keep := failureBudget(failed, body); keep < failed {
		listed = append(append([]Migration{}, shown.Rows[:keep]...), shown.Rows[failed:]...)
	}

	rows, styles := migrationTable(snap, listed, now)
	main := table.Table{Cols: migrationCols, Rows: rows, RowStyles: styles}
	out := lead + summary + "\n" + main.Render(w, mh-1)

	// Spend anything left over on the detail behind the unresolved count. This
	// is what zoom buys: the grid caps this pane at ContentHeight, which asks
	// only for the compact form, while a zoomed pane is handed the whole body
	// and lands here with rows to spare.
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

	lines, _ := inventoryLines(s, w)
	for i, ln := range lines {
		lines[i] = table.PadOrTrunc(ln, w)
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
}

// inventoryLines composes the census, one line per counted resource kind, fitted
// to width. A width of zero or less leaves the state breakdowns unconstrained; see
// [breakdown]. ok is false when there is no census to draw.
//
// Shared by renderInventory and the resources pane's extent methods.
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

// migrationStyle colors a row by what it needs from the reader.
//
// broken separates the two kinds of failure that used to look identical. A
// migration that failed and left its instance in ERROR is a server that is down
// and cannot be moved; one that failed while the instance kept running is a
// drain that did not complete. Both are worth a row, only one is worth an
// interrupt, and telling them apart at a glance is the question the operator
// was really asking of this pane.
func migrationStyle(status string, broken bool) lipgloss.Style {
	var plain lipgloss.Style
	switch strings.ToLower(status) {
	case "failed", "error":
		if broken {
			return tui.StyleErr
		}
		return tui.StyleWarn
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
