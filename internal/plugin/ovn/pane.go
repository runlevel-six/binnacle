package ovn

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// pane renders both databases' Raft state.
type pane struct {
	store *store.Store
}

func newPane(s *store.Store) *pane { return &pane{store: s} }

func (p *pane) ID() string             { return "ovn" }
func (p *pane) Title() string          { return "OVN Raft" }
func (p *pane) Priority() tui.Priority { return tui.P2Useful }
func (p *pane) MinWidth() int          { return 46 }
func (p *pane) MinHeight() int         { return 6 }
func (p *pane) HeightWeight() int      { return 2 }

// Group puts this pane in the shared "Cloud" frame; see [tui.GroupedPane].
func (p *pane) GroupID() string    { return "cloud" }
func (p *pane) GroupTitle() string { return "Cloud" }
func (p *pane) GroupOrder() int    { return 1 }

var raftCols = []table.Column{
	{Header: "DB"},
	{Header: "ROLE"},
	{Header: "TERM"},
	{Header: "LEADER"},
	{Header: "MEMBERS"},
	{Header: "NOTE", Stretch: true},
}

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading OVN…")
	}
	if state.Err != nil {
		return table.ErrorBody(w, h, state.Err)
	}
	if state.Tier != kube.TierFull {
		return table.Placeholder(w, h, "Raft detail unavailable — "+state.TierReason)
	}
	if len(state.Statuses) == 0 {
		return table.Placeholder(w, h, "no databases reported")
	}

	// Below the table's natural width the compact summary carries more than a
	// mangled table would.
	if w < 64 {
		return clip(strings.Join(Summary(state), "\n"), w, h)
	}

	cells := make([][]string, 0, len(state.Statuses))
	styles := make([][]lipgloss.Style, 0, len(state.Statuses))
	for _, st := range state.Statuses {
		cells = append(cells, rowFor(st))
		styles = append(styles, stylesFor(st))
	}

	body := table.Table{Cols: raftCols, Rows: cells, CellStyles: styles}
	detail := staleDetail(state)
	if detail == "" || h < len(cells)+3 {
		return body.Render(w, h)
	}
	return clip(body.Render(w, h-2)+"\n\n"+detail, w, h)
}

func rowFor(st ClusterStatus) []string {
	label := databaseLabel(st.Database)
	if st.Err != nil {
		return []string{label, "—", "—", "—", "—", "unreadable: " + st.Err.Error()}
	}

	leader := st.LeaderName()
	switch {
	case leader == "":
		leader = "none"
	case st.IsLeader():
		leader += " (self)"
	}

	// A fraction asserts that the numerator was checked. From a follower's view it
	// was not, so the count stands alone rather than reading as a clean 3/3 beside a
	// note saying nothing was verified.
	members := fmt.Sprintf("%d", len(st.Servers))
	if st.MemberViewTrusted() {
		healthy := 0
		for _, s := range st.Servers {
			if !st.Stale(s) {
				healthy++
			}
		}
		members = fmt.Sprintf("%d/%d", healthy, len(st.Servers))
	}
	return []string{
		label,
		orDash(st.Role),
		fmt.Sprintf("%d", st.Term),
		leader,
		members,
		note(st),
	}
}

// note is the one-line summary of what is wrong, or blank when nothing is.
func note(st ClusterStatus) string {
	switch {
	case !st.HasLeader():
		// Without a leader, writes to this database are failing right now.
		return "election in progress — writes failing"
	case len(st.StaleServers()) > 0:
		return fmt.Sprintf("%d member(s) not responding", len(st.StaleServers()))
	case !st.MemberViewTrusted():
		// Read from a follower, which knows the leader is alive and nothing about
		// the other members. Saying so beats implying the cluster was checked.
		return "members not checked — read from " + st.Pod
	case st.Uncommitted > 0 || st.Unapplied > 0:
		return fmt.Sprintf("%d uncommitted, %d unapplied", st.Uncommitted, st.Unapplied)
	}
	return ""
}

func stylesFor(st ClusterStatus) []lipgloss.Style {
	if st.Err != nil {
		return []lipgloss.Style{{}, {}, {}, {}, {}, tui.StyleWarn}
	}

	leaderStyle, memberStyle, noteStyle := tui.StyleOK, tui.StyleOK, lipgloss.Style{}
	if !st.HasLeader() {
		leaderStyle, noteStyle = tui.StyleErr, tui.StyleErr
	}
	if len(st.StaleServers()) > 0 {
		memberStyle = tui.StyleErr
		if noteStyle.GetForeground() == nil {
			noteStyle = tui.StyleWarn
		}
	}
	return []lipgloss.Style{
		{},
		roleStyle(st.Role),
		tui.StyleMuted,
		leaderStyle,
		memberStyle,
		noteStyle,
	}
}

// roleStyle colors a Raft role. Leader and follower are both normal; candidate
// means an election is under way.
func roleStyle(role string) lipgloss.Style {
	switch role {
	case "leader", "follower":
		return tui.StyleOK
	case "candidate":
		return tui.StyleWarn
	}
	return tui.StyleMuted
}

// staleDetail names the silent members and how long they have been quiet.
//
// The duration is the point: a Raft election timer here is about a second, so a
// member last heard from hours ago is not slow, it is gone — and its StatefulSet
// will still report every replica Ready, which is why this line exists.
func staleDetail(state State) string {
	var parts []string
	for _, st := range state.Statuses {
		for _, s := range st.StaleServers() {
			// The pod name, not the Raft ID: the ID identifies a member, but only
			// the pod name says which thing to go and look at.
			part := fmt.Sprintf("%s %s (%s",
				databaseLabel(st.Database), s.DisplayName(), humanDuration(s.LastMsg))
			// How far behind answers the follow-up question: whether the member is
			// merely unreachable or has actually stopped replicating.
			if behind, ok := st.Behind(s); ok && behind > 0 {
				part += fmt.Sprintf(", behind %d", behind)
			}
			parts = append(parts, part+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return tui.StyleErr.Render("Not responding: ") + strings.Join(parts, ", ")
}

// humanDuration renders a lag compactly.
//
// One significant figure past the unit is enough: the question a reader has is
// "seconds, minutes or hours", and "2.2h" answers it in fewer cells than "2h12m".
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}

// Summary renders one line per database in a compact form, for a narrow pane or a
// diagnostic where the table will not fit:
//
//	nb: leader=ovn-ovsdb-nb-1 term=317 lag: ovn-ovsdb-nb-2 2.2h
//	sb: leader=ovn-ovsdb-sb-0 term=317 members 3/3
func Summary(state State) []string {
	out := make([]string, 0, len(state.Statuses))
	for _, st := range state.Statuses {
		if st.Err != nil {
			out = append(out, fmt.Sprintf("%s: unreadable (%v)", databaseLabel(st.Database), st.Err))
			continue
		}

		leader := st.LeaderName()
		if leader == "" {
			leader = "none"
		}
		line := fmt.Sprintf("%s: leader=%s term=%d", databaseLabel(st.Database), leader, st.Term)

		if stale := st.StaleServers(); len(stale) > 0 {
			// Name every lagging member and by how much; a count alone would not
			// say which pod to look at.
			parts := make([]string, 0, len(stale))
			for _, s := range stale {
				parts = append(parts, fmt.Sprintf("%s %s", s.DisplayName(), humanDuration(s.LastMsg)))
			}
			line += " lag: " + strings.Join(parts, ", ")
		} else if st.MemberViewTrusted() {
			line += fmt.Sprintf(" members %d/%d", len(st.Servers), len(st.Servers))
		} else {
			// A follower knows the leader is alive and nothing about its peers.
			// Claiming 3/3 from that view is how the false alarm's mirror image
			// would look: a blind spot reported as health.
			line += fmt.Sprintf(" members unchecked (read from %s)", st.Pod)
		}
		out = append(out, line)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
