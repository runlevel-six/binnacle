package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// fake is a minimal pane, optionally declaring group membership.
type fake struct {
	id, title  string
	prio       Priority
	weight     int
	minH       int
	group      string
	groupTitle string
	order      int
	body       string
}

func (f fake) ID() string         { return f.id }
func (f fake) Title() string      { return f.title }
func (f fake) Priority() Priority { return f.prio }
func (f fake) MinWidth() int      { return 10 }
func (f fake) MinHeight() int     { return max(f.minH, 1) }
func (f fake) HeightWeight() int  { return max(f.weight, 1) }
func (f fake) Render(w, h int, _ bool) string {
	lines := make([]string, h)
	for i := range lines {
		lines[i] = f.body
	}
	return strings.Join(lines, "\n")
}

type grouped struct{ fake }

func (g grouped) GroupID() string    { return g.group }
func (g grouped) GroupTitle() string { return g.groupTitle }
func (g grouped) GroupOrder() int    { return g.order }

func TestGroup_CollapsesMembersInPlace(t *testing.T) {
	panes := []Pane{
		fake{id: "machines"},
		grouped{fake{id: "cilium", group: "network", groupTitle: "Network", order: 1}},
		fake{id: "ceph"},
		grouped{fake{id: "metallb", group: "network", groupTitle: "Network", order: 0}},
	}

	out := Group(panes)
	if len(out) != 3 {
		t.Fatalf("got %d panes, want 3 (two ungrouped plus one frame)", len(out))
	}
	// The group takes its first member's slot, so grouping must not reshuffle the
	// dashboard around it.
	if out[0].ID() != "machines" || out[2].ID() != "ceph" {
		t.Errorf("ungrouped panes moved: %s, %s", out[0].ID(), out[2].ID())
	}
	g, ok := out[1].(*GroupPane)
	if !ok {
		t.Fatalf("slot 1 is %T, want a *GroupPane", out[1])
	}
	if g.Title() != "Network" {
		t.Errorf("title = %q, want Network", g.Title())
	}
	// GroupOrder decides section order, not the order they happened to arrive in.
	members := g.Members()
	if len(members) != 2 || members[0].ID() != "metallb" || members[1].ID() != "cilium" {
		t.Errorf("members = %v, want metallb then cilium by GroupOrder", ids(members))
	}
}

// A group whose other members were never detected must still render. This is the
// ordinary case for a plugin pane: MetalLB present, Cilium absent.
func TestGroup_SingleMemberStillRenders(t *testing.T) {
	out := Group([]Pane{
		grouped{fake{id: "metallb", title: "MetalLB", group: "network", groupTitle: "Network", body: "x"}},
	})
	if len(out) != 1 {
		t.Fatalf("got %d panes, want 1", len(out))
	}
	g := out[0].(*GroupPane)
	body := g.Render(20, 4, false)
	if strings.Contains(body, "MetalLB") {
		t.Error("a group of one should not label its only section: the frame " +
			"title already names it")
	}
	if lipgloss.Height(body) != 4 {
		t.Errorf("height %d, want 4", lipgloss.Height(body))
	}
}

// The frame must be exactly the rectangle it was given, whatever its members
// return. A member that miscounts its lines would otherwise push the grid off the
// bottom of the terminal.
func TestGroupPane_ExactSize(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		fake{id: "openstack", title: "OpenStack", body: "os"},
		fake{id: "ovn", title: "OVN Raft", body: "ovn"},
	})
	for _, size := range []struct{ w, h int }{{40, 10}, {80, 3}, {20, 1}, {60, 17}} {
		out := g.Render(size.w, size.h, false)
		if got := lipgloss.Height(out); got != size.h {
			t.Errorf("%dx%d: height %d", size.w, size.h, got)
		}
	}
}

// Both sections must appear, each under its own label, at a height that fits them.
func TestGroupPane_LabelsEachSection(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		fake{id: "openstack", title: "OpenStack", body: "os-row"},
		fake{id: "ovn", title: "OVN Raft", body: "ovn-row"},
	})
	out := g.Render(40, 10, false)
	for _, want := range []string{"OpenStack", "os-row", "OVN Raft", "ovn-row"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered frame is missing %q:\n%s", want, out)
		}
	}
}

// A group's layout properties come from its members, or the frame would be sized
// for one pane while holding three.
func TestGroupPane_AggregatesLayoutProperties(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		fake{id: "a", prio: P2Useful, weight: 2, minH: 4},
		fake{id: "b", prio: P0Critical, weight: 3, minH: 5},
	})
	if got := g.Priority(); got != P0Critical {
		t.Errorf("priority = %v, want the most important member's", got)
	}
	// The hungriest member plus one for the second section's label, not the sum:
	// a section's weight must not silently pull height from other grid rows.
	if got := g.HeightWeight(); got != 4 {
		t.Errorf("weight = %d, want the hungriest member's plus one per extra section", got)
	}
	if got := g.MinHeight(); got != 11 {
		t.Errorf("min height = %d, want the members' sum plus a label each", got)
	}
}

func ids(ps []Pane) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID())
	}
	return out
}

// --- frame extents --------------------------------------------------------

// measured is a pane that declares both content extents.
type measured struct {
	fake
	width, height int
}

func (m measured) ContentWidth() int     { return m.width }
func (m measured) ContentHeight(int) int { return m.height }

// A frame that stacks its sections is as tall as all of them, plus a label each
// and a separator between neighbors.
func TestGroupPane_ContentHeightStacked(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		measured{fake: fake{id: "a", title: "A"}, width: 40, height: 6},
		measured{fake: fake{id: "b", title: "B"}, width: 30, height: 3},
	})
	// 1 label + 6, a separator, then 1 label + 3.
	if got := g.ContentHeight(80); got != 12 {
		t.Errorf("ContentHeight: got %d want 12", got)
	}
	if got := g.ContentWidth(); got != 40 {
		t.Errorf("ContentWidth: got %d want 40 (the widest section)", got)
	}
}

// A frame wide enough to flow is as tall as its taller column, not as tall as all
// of its sections — and it asks for both columns at once.
func TestGroupPane_ContentExtentsFlowed(t *testing.T) {
	members := []Pane{
		measured{fake: fake{id: "a", title: "A", weight: 1}, width: 40, height: 8},
		measured{fake: fake{id: "b", title: "B", weight: 1}, width: 30, height: 2},
		measured{fake: fake{id: "c", title: "C", weight: 1}, width: 20, height: 3},
		measured{fake: fake{id: "d", title: "D", weight: 1}, width: 25, height: 3},
	}
	g := NewGroupPane("network", "Network", members)

	// Equal weights split the four sections down the middle: [a b] and [c d].
	// Left: 1+8, separator, 1+2 = 13. Right: 1+3, separator, 1+3 = 9.
	if got := g.ContentHeight(200); got != 13 {
		t.Errorf("flowed ContentHeight: got %d want 13 (the taller column)", got)
	}
	// Left column wants 40, right wants 25, plus the gutter between them.
	if got := g.ContentWidth(); got != 40+innerGutter+25 {
		t.Errorf("flowed ContentWidth: got %d want %d", got, 40+innerGutter+25)
	}
	// Too narrow to flow, so the sections stack and the frame is as tall as all
	// four of them: four labels, four sections, three separators.
	if got := g.ContentHeight(20); got != 23 {
		t.Errorf("stacked ContentHeight: got %d want 23", got)
	}
}

// A group of one renders its member bare, so it has the member's extents and no
// label row of its own.
func TestGroupPane_ContentExtentsOfOne(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		measured{fake: fake{id: "a", title: "A"}, width: 40, height: 6},
	})
	if got := g.ContentHeight(80); got != 6 {
		t.Errorf("ContentHeight: got %d want 6", got)
	}
	if got := g.ContentWidth(); got != 40 {
		t.Errorf("ContentWidth: got %d want 40", got)
	}
}

// One member that cannot say makes the frame's answer a guess, and the layout
// handles "no answer" better than it handles a wrong one.
func TestGroupPane_ContentExtentsNeedEveryMember(t *testing.T) {
	g := NewGroupPane("cloud", "Cloud", []Pane{
		measured{fake: fake{id: "a", title: "A"}, width: 40, height: 6},
		fake{id: "b", title: "B"},
	})
	if got := g.ContentHeight(80); got != 0 {
		t.Errorf("ContentHeight: got %d want 0", got)
	}
	if got := g.ContentWidth(); got != 0 {
		t.Errorf("ContentWidth: got %d want 0", got)
	}
}
