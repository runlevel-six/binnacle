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
