package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
	ciliumstate "github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

const KeyState = ciliumstate.KeyState

type (
	IPAM       = ciliumstate.IPAM
	Hubble     = ciliumstate.Hubble
	Controllers = ciliumstate.Controllers
	MeshPeer   = ciliumstate.MeshPeer
	ClusterMesh = ciliumstate.ClusterMesh
	Status     = ciliumstate.Status
	State      = ciliumstate.State
)

type Provider struct{}

func NewProvider() *Provider { return &Provider{} }

func (p *Provider) Name() string { return "cilium" }

func (p *Provider) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{newPane(s)}
}

// pane renders Cilium's state, showing only what the current tier supports.
type pane struct {
	store *store.Store
}

func newPane(s *store.Store) *pane { return &pane{store: s} }

func (p *pane) ID() string             { return "cilium" }
func (p *pane) Title() string          { return "Cilium" }
func (p *pane) Priority() tui.Priority { return tui.P1Important }
func (p *pane) MinWidth() int          { return 40 }
func (p *pane) MinHeight() int         { return 6 }
func (p *pane) HeightWeight() int      { return 3 }

// Group puts this pane in the shared "Network" frame; see [tui.GroupedPane].
func (p *pane) GroupID() string    { return "network" }
func (p *pane) GroupTitle() string { return "Network" }
func (p *pane) GroupOrder() int    { return 0 }

// ContentWidth implements [tui.ContentWidthPane]: the longest status line.
func (p *pane) ContentWidth() int {
	return tui.WidestLine(p.lines())
}

// ContentHeight implements [tui.ContentHeightPane]. Cilium's status is a fixed
// handful of label-and-value rows — a few more mid-rollout, a few less on a reduced
// tier — and no height beyond them shows anything further.
func (p *pane) ContentHeight(int) int { return len(p.lines()) }

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading Cilium…")
	}
	if state.Err != nil {
		return table.ErrorBody(w, h, state.Err)
	}
	return clip(strings.Join(p.statusLines(state), "\n"), w, h)
}

// lines is [pane.statusLines] for the callers that only want to measure it, and
// answers nothing at all while the state is missing or failed — those bodies are
// placeholders sized to the tile, so they have no extent of their own to report.
func (p *pane) lines() []string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok || state.Err != nil {
		return nil
	}
	return p.statusLines(state)
}

// statusLines composes the pane's body, one label-and-value row at a time. Shared
// with the extent methods so what the pane declares and what it draws cannot diverge.
func (p *pane) statusLines(state State) []string {
	lines := []string{
		row("agents", agentText(state)),
	}

	if state.Tier == kube.TierFull {
		st := state.Status
		lines = append(lines,
			row("version", orUnknown(st.Version)),
			row("state", tui.StatusStyle(st.State).Render(orUnknown(st.State))),
			row("kube-proxy", st.KubeProxyText()),
			row("ipam", ipamText(st.IPAM, state.Pod)),
			row("hubble", hubbleText(st.Hubble)),
			row("controllers", controllerText(st.Controllers)),
		)
		if st.EncryptionMode != "" && !strings.EqualFold(st.EncryptionMode, "disabled") {
			lines = append(lines, row("encryption", st.EncryptionMode))
		}
		if len(st.ClusterMesh.Peers) > 0 {
			lines = append(lines, row("cluster mesh", meshText(st.ClusterMesh)))
		}
		if len(st.Unreadable) > 0 {
			lines = append(lines, row("unreadable",
				tui.StyleWarn.Render(strings.Join(st.Unreadable, ", "))))
		}
	} else {
		lines = append(lines, "", tui.StyleMuted.Render("detail unavailable — "+state.TierReason))
	}

	return lines
}

// labelWidth aligns the pane's label column. A constant rather than a parameter:
// every caller passed the same number, and a width that varied per row would
// defeat the alignment it exists for.
const labelWidth = 14

func row(label, value string) string {
	return tui.StyleMuted.Render(table.PadOrTrunc(label, labelWidth)) + " " + value
}

func agentText(s State) string {
	text := fmt.Sprintf("%d/%d ready", s.AgentsReady, s.AgentsDesired)
	if r := s.Rollout; r.Known() && !r.Converged() {
		text += tui.StyleAccent.Render(fmt.Sprintf("  %d/%d updated", r.Updated, r.Desired))
	}
	switch {
	case s.AgentsDesired == 0:
		return tui.StyleMuted.Render("none scheduled")
	case s.AgentsReady < s.AgentsDesired:
		return tui.StyleErr.Render(text)
	}
	return tui.StyleOK.Render(text)
}

func ipamText(i IPAM, pod string) string {
	if i.Total() == 0 {
		return tui.StyleMuted.Render("no allocations reported")
	}
	if !i.ExhaustionKnown() {
		return fmt.Sprintf("%d allocated %s", i.Used, tui.StyleMuted.Render("(on "+pod+")"))
	}

	pctText := fmt.Sprintf("%d%%", i.Percent())
	style := tui.StyleOK
	switch {
	case i.Percent() >= 90:
		style = tui.StyleErr
	case i.Percent() >= 75:
		style = tui.StyleWarn
	}
	return fmt.Sprintf("%d/%d %s %s",
		i.Used, i.Total(), style.Render(pctText), tui.StyleMuted.Render("(on "+pod+")"))
}

func hubbleText(hb Hubble) string {
	if !hb.Enabled {
		return tui.StyleMuted.Render("disabled")
	}
	return fmt.Sprintf("%s  %s",
		tui.StatusStyle(hb.State).Render(orUnknown(hb.State)),
		tui.StyleMuted.Render(fmt.Sprintf("%.0f flows/s", hb.FlowsPerSecond)))
}

func controllerText(c Controllers) string {
	if c.Total == 0 {
		return tui.StyleMuted.Render("none reported")
	}
	if c.Failing > 0 {
		return tui.StyleWarn.Render(fmt.Sprintf("%d failing of %d", c.Failing, c.Total))
	}
	return tui.StyleOK.Render(fmt.Sprintf("%d healthy", c.Total))
}

func meshText(m ClusterMesh) string {
	ready := 0
	for _, peer := range m.Peers {
		if peer.Ready {
			ready++
		}
	}
	style := tui.StyleOK
	if ready < len(m.Peers) {
		style = tui.StyleWarn
	}
	return fmt.Sprintf("%s  %s",
		style.Render(fmt.Sprintf("%d/%d peers ready", ready, len(m.Peers))),
		tui.StyleMuted.Render(fmt.Sprintf("%d global services", m.GlobalServices)))
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
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
