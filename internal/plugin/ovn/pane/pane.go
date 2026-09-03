// Package pane renders OVN's terminal view.
//
// Separate from the plugin beside it so the collector carries no dependency on
// a renderer — the same split every subsystem makes, and the reason a web
// server can link the data layer without linking a terminal library.
package pane

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
	ovnstate "github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

const KeyState = ovnstate.KeyState

type (
	Server        = ovnstate.Server
	ClusterStatus = ovnstate.ClusterStatus
	Database      = ovnstate.Database
	State         = ovnstate.State
	Component     = ovnstate.Component
)

type Provider struct{}

func NewProvider() *Provider { return &Provider{} }

func (p *Provider) Name() string { return "ovn" }

func (p *Provider) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{newPane(s), newRolloutPane(s)}
}

// databaseLabel shortens a database name for a cell.
func databaseLabel(name string) string {
	for _, db := range ovnstate.Databases {
		if db.Name == name {
			return db.Label
		}
	}
	return name
}

// PendingComponents returns the families with pods still to update, the ones
// needing an operator first.
func PendingComponents(cs []Component) []Component {
	var out []Component
	for _, c := range cs {
		if !c.Converged() {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Manual != out[j].Manual {
			return out[i].Manual
		}
		return out[i].Stale() > out[j].Stale()
	})
	return out
}

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

func (p *pane) GroupID() string    { return "network" }
func (p *pane) GroupTitle() string { return "Network" }
func (p *pane) GroupOrder() int    { return 2 }

var raftCols = []table.Column{
	{Header: "DB"},
	{Header: "ROLE"},
	{Header: "TERM"},
	{Header: "LEADER"},
	{Header: "MEMBERS"},
	{Header: "NOTE", Stretch: true, Transient: true},
}

func (p *pane) ContentWidth() int {
	cells, _, _ := p.content()
	if len(cells) == 0 {
		return 0
	}
	return table.AppetiteWidth(raftCols, cells)
}

const compactWidth = 64

func (p *pane) ContentHeight(bodyWidth int) int {
	cells, _, detail := p.content()
	if len(cells) == 0 {
		return 0
	}
	if bodyWidth < compactWidth {
		return len(Summary(p.state()))
	}
	h := len(cells) + 1
	if detail != "" {
		h += 2
	}
	return h
}

func (p *pane) state() State {
	state, _ := store.Get[State](p.store, KeyState)
	return state
}

func (p *pane) content() (cells [][]string, styles [][]lipgloss.Style, detail string) {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || state.Err != nil || state.Tier != kube.TierFull || len(state.Statuses) == 0 {
		return nil, nil, ""
	}
	cells = make([][]string, 0, len(state.Statuses))
	styles = make([][]lipgloss.Style, 0, len(state.Statuses))
	for _, st := range state.Statuses {
		cells = append(cells, rowFor(st))
		styles = append(styles, stylesFor(st))
	}
	return cells, styles, staleDetail(state)
}

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

	if w < compactWidth {
		return clip(strings.Join(Summary(state), "\n"), w, h)
	}

	cells, styles, detail := p.content()
	body := table.Table{Cols: raftCols, Rows: cells, CellStyles: styles}
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

func note(st ClusterStatus) string {
	switch {
	case !st.HasLeader():
		return "election in progress — writes failing"
	case len(st.StaleServers()) > 0:
		return fmt.Sprintf("%d member(s) not responding", len(st.StaleServers()))
	case !st.MemberViewTrusted():
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

func roleStyle(role string) lipgloss.Style {
	switch role {
	case "leader", "follower":
		return tui.StyleOK
	case "candidate":
		return tui.StyleWarn
	}
	return tui.StyleMuted
}

func staleDetail(state State) string {
	var parts []string
	for _, st := range state.Statuses {
		for _, s := range st.StaleServers() {
			part := fmt.Sprintf("%s %s (%s",
				databaseLabel(st.Database), s.DisplayName(), humanDuration(s.LastMsg))
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
			parts := make([]string, 0, len(stale))
			for _, s := range stale {
				parts = append(parts, fmt.Sprintf("%s %s", s.DisplayName(), humanDuration(s.LastMsg)))
			}
			line += " lag: " + strings.Join(parts, ", ")
		} else if st.MemberViewTrusted() {
			line += fmt.Sprintf(" members %d/%d", len(st.Servers), len(st.Servers))
		} else {
			line += fmt.Sprintf(" members unchecked (read from %s)", st.Pod)
		}
		out = append(out, line)
	}

	for _, c := range state.Components {
		line := fmt.Sprintf("%s %d/%d up to date", c.Name, c.Updated, c.Desired)
		if c.Manual {
			line += " (manual: OnDelete)"
		}
		if nodes, more := kube.ShortNodeNames(c.StaleNodes, 4); len(nodes) > 0 {
			line += " pending: " + strings.Join(nodes, ", ")
			if more > 0 {
				line += fmt.Sprintf(", +%d", more)
			}
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
