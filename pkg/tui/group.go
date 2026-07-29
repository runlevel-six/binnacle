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
	if h < len(g.members)*4 {
		gap = 0
	}
	avail := h - gap*(len(g.members)-1)
	total := 0
	for _, m := range g.members {
		total += max(m.HeightWeight(), 1)
	}

	lines := make([]string, 0, h)
	for i, m := range g.members {
		if i > 0 {
			for range gap {
				lines = append(lines, "")
			}
		}
		// A label plus one row is the floor: a section squeezed below that is
		// better dropped than shown as a heading with nothing under it.
		sectionH := max(avail*max(m.HeightWeight(), 1)/total, 2)
		if i == len(g.members)-1 {
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
	// Trim or pad to exactly h: a member that returned the wrong line count must
	// not be able to push the grid off the bottom of the terminal.
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
