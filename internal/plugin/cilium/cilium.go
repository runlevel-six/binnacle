// Package cilium reports the Cilium CNI's state.
//
// This is the tiered plugin shape. Agent readiness comes from the DaemonSet and
// needs no special permission; everything else comes from `cilium status -o json`
// run inside an agent pod, which needs `pods/exec`. Without that permission the
// plugin still reports agent readiness rather than failing, because an operator
// with tighter RBAC than the author's should get a thinner pane, not an error.
//
// # Tested against
//
// The status parser is exercised against captured output from Cilium 1.14, 1.15,
// 1.16 and 1.19 (see testdata). The schema is unversioned and has changed shape
// repeatedly — the IPAM block alone has four known forms — so every field is
// optional and a shape we have not seen degrades that one cell rather than the
// whole pane.
package cilium

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// Name is the plugin's registration name.
const Name = "cilium"

// KeyState holds a State.
const KeyState = "cilium/state"

// TestedVersions records the releases the parser has fixtures for. Reported in
// diagnostics so a parse failure on an untested release is self-explaining.
var TestedVersions = []string{"1.14", "1.15", "1.16", "1.19"}

// pollInterval is how often `cilium status` is re-run. Each poll is an exec into
// a pod, so this is deliberately slower than an informer would be.
const pollInterval = 20 * time.Second

// IPAM is one agent's view of pod-address allocation.
//
// These numbers are per-node, not cluster-wide: a pane must say which agent they
// came from or a reader will take one node's exhaustion for the cluster's.
type IPAM struct {
	Used      int
	Available int
}

// Total is the implied pool size. Cilium reports no total because a pool can
// fragment across CIDRs, but used plus available is the figure that answers "are
// we about to run out".
func (i IPAM) Total() int { return i.Used + i.Available }

// ExhaustionKnown reports whether Available is meaningful.
//
// The oldest status shape lists allocated addresses with no remaining count, so
// Available is zero because it is unknown, not because the pool is full.
// Presenting that as 100% used would be a false alarm.
func (i IPAM) ExhaustionKnown() bool { return i.Available > 0 || i.Used == 0 }

// Percent returns used as a percentage of the pool, or -1 when unknown.
func (i IPAM) Percent() int {
	if !i.ExhaustionKnown() || i.Total() == 0 {
		return -1
	}
	return i.Used * 100 / i.Total()
}

// Hubble is the observability layer's state.
type Hubble struct {
	Enabled   bool
	State     string
	SeenFlows int64
	// FlowsPerSecond is derived between polls, since SeenFlows is a lifetime
	// counter. A lifetime average would understate a current spike by orders of
	// magnitude on a long-running agent.
	FlowsPerSecond float64
}

// Controllers counts Cilium's internal controllers on the agent polled.
type Controllers struct {
	Total   int
	Failing int
}

// MeshPeer is one cluster-mesh peer.
type MeshPeer struct {
	Name  string
	Ready bool
}

// ClusterMesh is the cross-cluster view. No peers means mesh is not enabled.
type ClusterMesh struct {
	Peers          []MeshPeer
	GlobalServices int
}

// Status is what one agent reports.
type Status struct {
	Version              string
	State                string
	KubeProxyReplacement string
	EncryptionMode       string
	IPAM                 IPAM
	Hubble               Hubble
	Controllers          Controllers
	ClusterMesh          ClusterMesh
	// Unreadable names the status sections that could not be decoded, so a
	// missing cell is reported as unreadable rather than as absent or healthy.
	Unreadable []string
}

// State is everything the plugin publishes.
type State struct {
	// Tier is how much of the below is populated.
	Tier kube.Tier
	// AgentsReady and AgentsDesired come from the DaemonSet and are available at
	// every tier.
	AgentsReady   int32
	AgentsDesired int32
	// Status is populated only at the full tier.
	Status Status
	// Pod names the agent Status came from, so per-node figures can be labeled.
	Pod string
	// TierReason explains a reduced tier, so a thin pane says why it is thin.
	TierReason string
	UpdatedAt  time.Time
	Err        error
}

// Settings is the plugin's profile configuration.
type Settings struct {
	Namespace     string
	DaemonSetName string
	PodSelector   string
	Container     string
}

// Defaults are the upstream chart's conventions.
func Defaults() Settings {
	return Settings{
		Namespace:     "kube-system",
		DaemonSetName: "cilium",
		PodSelector:   "k8s-app=cilium",
		Container:     "cilium-agent",
	}
}

// SettingsFrom reads a profile's plugin block over the defaults.
func SettingsFrom(raw map[string]any) Settings {
	s := Defaults()
	for key, dst := range map[string]*string{
		"namespace":      &s.Namespace,
		"daemonset_name": &s.DaemonSetName,
		"pod_selector":   &s.PodSelector,
		"container":      &s.Container,
	} {
		if v, ok := raw[key].(string); ok && v != "" {
			*dst = v
		}
	}
	return s
}

// Plugin observes Cilium.
type Plugin struct {
	client   *kube.Client
	settings Settings

	tier       kube.Tier
	tierReason string
	// lastFlows and lastPoll derive the flow rate between polls.
	lastFlows int64
	lastPoll  time.Time
}

// New builds the plugin.
func New(c *kube.Client, settings Settings) *Plugin {
	return &Plugin{client: c, settings: settings}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return Name }

// Detect reports whether Cilium is present, and decides the starting tier.
//
// The DaemonSet is the probe rather than a CRD, because Cilium's CRDs are
// installed by some other tools too, and the agent is what the plugin actually
// reads.
//
// The tier decided here is provisional except for a permission denial. Detection
// runs once, at startup, and every other reason an exec can fail is a fact about
// this moment rather than about the cluster: an agent still starting, a node
// draining, a pod terminating. Recording one of those as the tier would leave the
// pane saying "no detail" for the rest of the session — which is exactly what
// happened during a live upgrade when detection landed on a pod whose node had
// just gone down. Nothing decided here is final: poll re-derives all of it.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	if !p.client.HasDaemonSet(ctx, p.settings.Namespace, p.settings.DaemonSetName) {
		p.tier = kube.TierAbsent
		return false, nil
	}

	pod, err := p.client.RunningPod(ctx, p.settings.Namespace, p.settings.PodSelector)
	if err != nil {
		p.tier = kube.TierInformer
		p.tierReason = "no agent pod ready to query yet"
		// Present, not reachable — reported, and still active. See plugin.Source.
		return true, err
	}
	ok, forbidden := p.client.ExecProbe(ctx, p.settings.Namespace, pod, p.settings.Container)
	switch {
	case ok:
		p.tier = kube.TierFull
	case forbidden:
		p.tier = kube.TierInformer
		p.tierReason = "no pods/exec permission on " + p.settings.Namespace
	default:
		// Not a verdict: name the pod that did not answer, and let poll try
		// another one.
		p.tier = kube.TierInformer
		p.tierReason = "agent pod " + pod + " did not answer"
	}
	return true, nil
}

// Run polls until ctx is canceled.
func (p *Plugin) Run(ctx context.Context, s *store.Store) error {
	publish := func() { s.Put(KeyState, p.poll(ctx)) }
	publish()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			publish()
		}
	}
}

func (p *Plugin) poll(ctx context.Context) State {
	state := State{Tier: p.tier, TierReason: p.tierReason, UpdatedAt: time.Now()}

	// Agent readiness first: it is available at every tier and is the single most
	// useful fact, so a later failure must not discard it.
	if ds, err := p.client.Typed.AppsV1().DaemonSets(p.settings.Namespace).
		Get(ctx, p.settings.DaemonSetName, metav1.GetOptions{}); err == nil {
		state.AgentsReady = ds.Status.NumberReady
		state.AgentsDesired = ds.Status.DesiredNumberScheduled
	} else {
		state.Err = fmt.Errorf("get daemonset %s/%s: %w",
			p.settings.Namespace, p.settings.DaemonSetName, err)
		return state
	}

	// The detail read is attempted every poll, whatever happened last time and
	// whatever detection concluded. Nothing here is remembered as final — not an
	// unreachable pod, and not a permission denial either; see [kube.Forbidden].
	pods, err := p.client.PodCandidates(ctx, p.settings.Namespace, p.settings.PodSelector, nil)
	if err != nil {
		// An agent restarting is transient, so this reduces the reported tier for
		// this poll without changing the plugin's own tier.
		state.Tier = kube.TierInformer
		state.TierReason = "no agent pod ready right now"
		return state
	}

	out, pod, err := p.client.ExecFirstOf(ctx, p.settings.Namespace, pods,
		p.settings.Container, []string{"cilium", "status", "-o", "json"})
	if err != nil {
		state.Tier = kube.TierInformer
		if kube.Forbidden(err) {
			state.TierReason = "no pods/exec permission on " + p.settings.Namespace
		} else {
			state.TierReason = fmt.Sprintf("no agent pod answered (tried %d)", len(pods))
		}
		return state
	}
	state.Pod = pod
	// A pod answered, so whatever detection concluded is out of date.
	p.tier = kube.TierFull
	p.tierReason = ""
	state.Tier = kube.TierFull
	state.TierReason = ""

	status, err := ParseStatus([]byte(out))
	if err != nil {
		// A shape we cannot read is worth naming with the versions we do handle,
		// so the reader knows to report it rather than assume Cilium is broken.
		state.Tier = kube.TierInformer
		state.TierReason = fmt.Sprintf("could not parse cilium status (tested against %v): %v",
			TestedVersions, err)
		return state
	}

	status.Hubble.FlowsPerSecond = p.flowRate(status.Hubble.SeenFlows, state.UpdatedAt)
	state.Status = status
	return state
}

// flowRate derives flows per second from the change in the lifetime counter.
//
// A lifetime average would understate a current spike by orders of magnitude on a
// long-running agent, which is the opposite of what the number is for. The first
// poll has no previous sample and reports zero rather than guessing.
func (p *Plugin) flowRate(seen int64, now time.Time) float64 {
	defer func() {
		p.lastFlows = seen
		p.lastPoll = now
	}()

	if p.lastPoll.IsZero() || seen < p.lastFlows {
		// A counter that went backwards means the agent restarted; treat it as a
		// fresh baseline rather than reporting a negative rate.
		return 0
	}
	elapsed := now.Sub(p.lastPoll).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(seen-p.lastFlows) / elapsed
}

// Cells implements plugin.BannerProvider.
func (p *Plugin) Cells(s *store.Store) []tui.BannerCell {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		return nil
	}

	cell := tui.BannerCell{Name: "Cilium"}
	switch {
	case state.Err != nil:
		cell.Status = tui.BannerWarn
		cell.Detail = "read error"
	case state.AgentsDesired > 0 && state.AgentsReady < state.AgentsDesired:
		cell.Status = tui.BannerErr
		cell.Detail = fmt.Sprintf("%d/%d agents", state.AgentsReady, state.AgentsDesired)
	case state.Status.Controllers.Failing > 0:
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d controller(s) failing", state.Status.Controllers.Failing)
	case state.Status.IPAM.Percent() >= 90:
		// Pod-address exhaustion stops new pods scheduling, and nothing else on
		// screen would explain why.
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("IPAM %d%%", state.Status.IPAM.Percent())
	default:
		cell.Status = tui.BannerOK
	}
	return []tui.BannerCell{cell}
}

// Panes implements plugin.PaneProvider.
func (p *Plugin) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{newPane(s)}
}
