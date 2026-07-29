package cilium

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

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

// Render implements tui.Pane.
func (p *pane) Render(w, h int, _ bool) string {
	state, ok := store.Get[State](p.store, KeyState)
	if !ok {
		return table.Placeholder(w, h, "loading Cilium…")
	}
	if state.Err != nil {
		return table.ErrorBody(w, h, state.Err)
	}

	lines := []string{
		row("agents", agentText(state)),
	}

	if state.Tier == kube.TierFull {
		st := state.Status
		lines = append(lines,
			row("version", orUnknown(st.Version)),
			row("state", tui.StatusStyle(st.State).Render(orUnknown(st.State))),
			row("kube-proxy", kubeProxyText(st.KubeProxyReplacement)),
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
			// Naming the sections we could not read distinguishes "this release
			// changed shape" from "this feature is off", which look identical
			// otherwise.
			lines = append(lines, row("unreadable",
				tui.StyleWarn.Render(strings.Join(st.Unreadable, ", "))))
		}
	} else {
		// A reduced tier says so and says why, rather than silently showing less
		// and leaving the reader to wonder what is missing.
		lines = append(lines, "", tui.StyleMuted.Render("detail unavailable — "+state.TierReason))
	}

	return clip(strings.Join(lines, "\n"), w, h)
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
	switch {
	case s.AgentsDesired == 0:
		return tui.StyleMuted.Render("none scheduled")
	case s.AgentsReady < s.AgentsDesired:
		return tui.StyleErr.Render(text)
	}
	return tui.StyleOK.Render(text)
}

// ipamText labels the pod it came from, because these figures are per-node and an
// unlabelled percentage reads as cluster-wide.
func ipamText(i IPAM, pod string) string {
	if i.Total() == 0 {
		return tui.StyleMuted.Render("no allocations reported")
	}
	if !i.ExhaustionKnown() {
		// This release does not report a remaining count, so a percentage would be
		// a fabrication.
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

// kubeProxyText says what happened to kube-proxy, rather than echoing Cilium's
// raw mode.
//
// The field is `kube-proxy-replacement`, and rendering its value beside the label
// "kube-proxy" reverses the meaning: a cluster where Cilium has *replaced*
// kube-proxy reports "True", which reads as kube-proxy being present and enabled.
// Since replacing kube-proxy is one of the main reasons to run Cilium, and the
// clusters that do it no longer have kube-proxy installed at all, that is the
// worst possible way to be wrong.
//
// An unrecognized mode is passed through rather than guessed at: Cilium has
// shipped "Strict", "Partial", "Probe" and "Disabled" over the years, and a
// future one should appear verbatim instead of being flattened into a yes or no.
func kubeProxyText(mode string) string {
	switch strings.ToLower(mode) {
	case "":
		return orUnknown(mode)
	case "true", "strict":
		return "replaced by Cilium"
	case "false", "disabled":
		// Deliberately says nothing about whether kube-proxy is running: Cilium
		// reports only that it is not replacing it, and claiming more than that
		// would be inventing an observation.
		return "not replaced"
	case "partial":
		return "partially replaced"
	}
	return mode
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
