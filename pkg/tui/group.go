package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// GroupedPane is implemented by panes that share one frame with others.
//
// Two subsystems that an operator thinks about together do not each need their
// own border and title: MetalLB and Cilium are both "the network", OpenStack and
// OVN are both "the cloud". Each says little enough to fit in a fraction of a
// column, so giving them a frame apiece spends four rows of chrome on eight rows
// of content and scatters one question across two places.
//
// A pane declares which group it belongs to and where it sits in it; the
// collapsing happens in [Group]. Declaring membership rather than assembling the
// composite by hand is what keeps this working as plugins come and go — a group
// whose other member was not detected renders as a single section, and one whose
// members are all absent never appears.
type GroupedPane interface {
	Pane
	// GroupID identifies the shared frame. Panes sharing an ID share a frame.
	GroupID() string
	// GroupTitle is the frame's title. Members should agree; the first wins.
	GroupTitle() string
	// GroupOrder places this pane within the frame, low to high.
	GroupOrder() int
}

// GroupPane renders several panes as labeled sections under one frame.
//
// It delegates to the members' own Render, so a section shows exactly what that
// pane would have shown alone. Nothing is reimplemented and nothing is curated
// away — which matters, because a composite that re-derived its content would
// drift from the pane it replaced and the drift would be invisible.
type GroupPane struct {
	id      string
	title   string
	members []Pane
}

// NewGroupPane builds a composite from members already in display order.
func NewGroupPane(id, title string, members []Pane) *GroupPane {
	return &GroupPane{id: id, title: title, members: members}
}

// Members returns the panes this frame holds, in display order.
func (g *GroupPane) Members() []Pane { return append([]Pane(nil), g.members...) }

// ID is the shared frame's identity, taken from the group its members declared.
func (g *GroupPane) ID() string { return g.id }

// Title is the frame's title, which names the group rather than any one member.
func (g *GroupPane) Title() string { return g.title }

// Priority is the most important member's, so a group is kept or dropped by its
// best reason to be on screen rather than its worst.
func (g *GroupPane) Priority() Priority {
	out := P3Optional
	for _, m := range g.members {
		if m.Priority() < out {
			out = m.Priority()
		}
	}
	return out
}

// MinWidth is the widest member's: every section renders at the frame's width, so
// the frame cannot be narrower than the most demanding one.
func (g *GroupPane) MinWidth() int {
	out := 0
	for _, m := range g.members {
		out = max(out, m.MinWidth())
	}
	return out
}

// MinHeight is the sum of the members' minimums plus a label row each, since
// they are stacked rather than overlaid.
func (g *GroupPane) MinHeight() int {
	out := 0
	for _, m := range g.members {
		out += m.MinHeight() + 1
	}
	return out
}

// HeightWeight is the hungriest member's, plus one per extra section for its
// label and separator.
//
// Deliberately not the sum. A member's weight has to divide the frame between
// sections, and if it also summed into the frame's pull on the grid then giving
// one section more room inside the frame would silently take height from every
// other row on screen — one number doing two jobs, with the second effect
// invisible. This keeps a merged frame competing for grid height roughly as a
// single pane does, which is what the operator's ordering asks for: a frame of
// secondary subsystems should not out-pull the pane you look at first.
func (g *GroupPane) HeightWeight() int {
	out := 0
	for _, m := range g.members {
		out = max(out, m.HeightWeight())
	}
	return max(out, 1) + max(len(g.members)-1, 0)
}

// innerGutter is the blank column between a frame's flowed columns. One column,
// because the sections already carry their own labels and a wider channel would
// read as two frames that forgot their borders.
const innerGutter = 1

// flowMinMembers is the section count at which a frame prefers two columns.
//
// Four, arrived at by measurement rather than by reasoning about averages. Three
// sections stack acceptably in one column — measured on a live cloud, the Cloud
// frame's agents, versions and resources all rendered in full — while the second
// grid column a flowing frame needs has to come from somewhere. At four columns
// it came out of the row itself: a two-column Network beside a two-column Cloud
// filled the row and pushed the migrations pane onto a row of its own, where it
// spread seventy columns of table across three hundred.
//
// So the threshold is where stacking genuinely stops working rather than where it
// starts to be tight. A four-section frame in one column gives each a quarter,
// which is when a table renders its header and then "+ 2 more".
const flowMinMembers = 4

// ColSpan implements [tui.WidePane].
//
// A frame that will flow its sections has to be given the width to flow into,
// and the grid allocates columns before anything renders — so the request is made
// here from the member count rather than discovered at draw time. A frame with
// too few sections to flow asks for one column and is placed exactly as before.
func (g *GroupPane) ColSpan() int {
	if len(g.members) < flowMinMembers {
		return 1
	}
	return 2
}

// MinColsForSpan implements [SpanFloorPane]. A frame's second column lets it flow
// its sections; it is never required, so it is surrendered on any grid that
// cannot spare it. Four is the point at which a two-column frame still leaves two
// columns beside it — the same threshold row spans use.
func (g *GroupPane) MinColsForSpan() int { return 4 }

// flowCols is how many inner columns to use at this width.
//
// The width test is against the widest member rather than the average: every
// section renders at its column's width, so one demanding member sets the floor
// for all of them. A frame that asked for two grid columns and did not get them —
// a narrow terminal, a row with no room — falls back to stacking rather than
// flowing into columns too narrow to hold anything.
func (g *GroupPane) flowCols(w int) int {
	if len(g.members) < flowMinMembers {
		return 1
	}
	widest := 0
	for _, m := range g.members {
		widest = max(widest, m.MinWidth())
	}
	if w >= widest*2+innerGutter {
		return 2
	}
	return 1
}

// Render stacks the members, each under its own label.
//
// Height is divided by [Pane.HeightWeight], not evenly. Sections are not
// equal-sized things: Cilium reports eight rows of agent, IPAM, Hubble and
// controller state where MetalLB reports a pool table and a speaker count, and an
// even split clipped the first while leaving the second half empty. Weight is
// already every pane's declaration of how much vertical space it can use, so the
// same number that sizes a pane's own grid row sizes its section here.
//
// The split is stable across polls because it comes from declared weights rather
// than from current content — a section that resized as its neighbour's data
// changed would make a reader re-find it every twenty seconds.
func (g *GroupPane) Render(w, h int, focused bool) string {
	if len(g.members) == 0 || h <= 0 {
		return strings.TrimRight(strings.Repeat(strings.Repeat(" ", max(w, 0))+"\n", max(h, 0)), "\n")
	}
	if len(g.members) == 1 {
		// A group of one is just that pane. No label, because the frame's own
		// title already names it — a section header would repeat it.
		return g.members[0].Render(w, h, focused)
	}

	// Two inner columns when the frame is wide enough to hold them. The split
	// point is chosen to balance the columns by weight but is always contiguous,
	// so reading order stays down the left column and then down the right — the
	// same order the single-column frame had, which is what keeps a section from
	// moving when a terminal is resized across the threshold.
	if cols := g.flowCols(w); cols == 2 {
		half := g.splitAt()
		leftW := (w - innerGutter) / 2
		rightW := w - innerGutter - leftW
		left := g.renderStack(g.members[:half], leftW, h)
		right := g.renderStack(g.members[half:], rightW, h)
		return joinColumns(left, right, leftW, rightW, h)
	}
	return g.renderStack(g.members, w, h)
}

// splitAt returns the index where the members divide between the two columns.
//
// Balanced by summed [Pane.HeightWeight] rather than by count, because sections
// are not equal-sized things: splitting three members down the middle puts two
// tables on the left and one status block alone on the right, which leaves the
// right column half empty while the left one clips. Weight is already every
// pane's declaration of how much vertical space it can use, so the same number
// that sizes a section inside a column decides which column it lands in.
//
// The split is contiguous, so declaration order is preserved.
func (g *GroupPane) splitAt() int {
	total := 0
	for _, m := range g.members {
		total += max(m.HeightWeight(), 1)
	}

	best, bestDiff := 1, -1
	running := 0
	// Both columns must get at least one member, so the candidate split points
	// stop one short of the end.
	for i := 0; i < len(g.members)-1; i++ {
		running += max(g.members[i].HeightWeight(), 1)
		diff := total - 2*running
		if diff < 0 {
			diff = -diff
		}
		if bestDiff < 0 || diff < bestDiff {
			best, bestDiff = i+1, diff
		}
	}
	return best
}

// joinColumns places two rendered blocks side by side, padding each to its own
// width so a short line in one cannot pull the other's column leftwards.
func joinColumns(left, right string, leftW, rightW, h int) string {
	l, r := strings.Split(left, "\n"), strings.Split(right, "\n")
	lines := make([]string, 0, h)
	for i := range h {
		var lp, rp string
		if i < len(l) {
			lp = l[i]
		}
		if i < len(r) {
			rp = r[i]
		}
		lines = append(lines, padTo(lp, leftW)+strings.Repeat(" ", innerGutter)+padTo(rp, rightW))
	}
	return strings.Join(lines, "\n")
}

// padTo pads a line to width, measuring in cells rather than bytes so styled
// content lands in the right column.
func padTo(s string, width int) string {
	if n := lipgloss.Width(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// renderStack draws members top to bottom under their labels, filling exactly
// w x h.
func (g *GroupPane) renderStack(members []Pane, w, h int) string {
	if len(members) == 0 || h <= 0 {
		return strings.TrimRight(strings.Repeat(strings.Repeat(" ", max(w, 0))+"\n", max(h, 0)), "\n")
	}
	if len(members) == 1 {
		// One section in this column still gets its label: unlike a group of one,
		// the frame's title names the group and not this member.
		th := CurrentTheme()
		label := lipgloss.NewStyle().Foreground(th.Muted).Bold(true)
		if th.Grounded() {
			label = label.Background(th.PaneBG)
		}
		lines := append([]string{label.Render(th.Label(members[0].Title()))},
			strings.Split(members[0].Render(w, h-1, false), "\n")...)
		return clampLines(lines, h)
	}

	th := CurrentTheme()
	label := lipgloss.NewStyle().Foreground(th.Muted).Bold(true)
	if th.Grounded() {
		label = label.Background(th.PaneBG)
	}

	// A blank row between sections, so two tables under one frame read as two
	// things rather than one table that changed its mind about its columns.
	//
	// It is dropped when the frame is too short to spare it. A separator is worth
	// a row of whitespace when there is whitespace going spare and never worth a
	// row of data: this frame sits in the lowest-priority row, and on a large
	// cluster it is already the first place height is taken from.
	gap := 1
	if h < len(members)*4 {
		gap = 0
	}
	avail := h - gap*(len(members)-1)
	total := 0
	for _, m := range members {
		total += max(m.HeightWeight(), 1)
	}

	lines := make([]string, 0, h)
	for i, m := range members {
		if i > 0 {
			for range gap {
				lines = append(lines, "")
			}
		}
		// A label plus one row is the floor: a section squeezed below that is
		// better dropped than shown as a heading with nothing under it.
		sectionH := max(avail*max(m.HeightWeight(), 1)/total, 2)
		if i == len(members)-1 {
			// The last section absorbs the rounding remainder, so the frame is
			// filled to exactly the height it was given.
			sectionH = h - len(lines)
		}
		if sectionH <= 0 {
			break
		}
		lines = append(lines, label.Render(th.Label(m.Title())))
		if bodyH := sectionH - 1; bodyH > 0 {
			lines = append(lines, strings.Split(m.Render(w, bodyH, false), "\n")...)
		}
	}
	return clampLines(lines, h)
}

// clampLines trims or pads to exactly h lines. A member that returned the wrong
// line count must not be able to push the grid off the bottom of the terminal.
func clampLines(lines []string, h int) string {
	for len(lines) > h {
		lines = lines[:len(lines)-1]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// Group collapses [GroupedPane] members into [GroupPane] composites.
//
// A group takes the position of its first member in the input order, so grouping
// does not reshuffle the dashboard. Panes declaring no group pass through
// untouched.
func Group(panes []Pane) []Pane {
	type collected struct {
		title   string
		members []GroupedPane
		at      int
	}
	groups := map[string]*collected{}
	var order []string

	out := make([]Pane, 0, len(panes))
	for _, p := range panes {
		gp, ok := p.(GroupedPane)
		if !ok || gp.GroupID() == "" {
			out = append(out, p)
			continue
		}
		id := gp.GroupID()
		if groups[id] == nil {
			groups[id] = &collected{title: gp.GroupTitle(), at: len(out)}
			order = append(order, id)
			// Reserve the slot; filled in below once every member is known.
			out = append(out, nil)
		}
		groups[id].members = append(groups[id].members, gp)
	}

	for _, id := range order {
		c := groups[id]
		sort.SliceStable(c.members, func(i, j int) bool {
			return c.members[i].GroupOrder() < c.members[j].GroupOrder()
		})
		members := make([]Pane, 0, len(c.members))
		for _, m := range c.members {
			members = append(members, m)
		}
		out[c.at] = NewGroupPane(id, c.title, members)
	}
	return out
}
