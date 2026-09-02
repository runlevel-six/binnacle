// Package ceph reports a Rook-managed Ceph cluster's health.
//
// Detail comes from `ceph -s --format=json` run inside a tools pod, so this is a
// tiered plugin: without `pods/exec` it reports what the CephCluster CR alone
// says, which is a health enum and nothing else.
//
// # Tested against
//
// The parser is exercised against captured output from a production Reef cluster
// (see testdata), including the summary-only `mgrmap` that current releases emit.
package ceph

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
	cephstate "github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/health"
)

// Name is the plugin's registration name.
const Name = "ceph"

// KeyState holds a State.
const KeyState = cephstate.KeyState

// pollInterval is how often `ceph -s` is re-run: one or two execs per poll.
const pollInterval = 20 * time.Second

// The Ceph state types live in pkg/subsystem/ceph so a consumer outside this
// module can read them. Aliased here so this package's own code, and any
// profile referring to these names, are unchanged.
type (
	// Check is an alias for [cephstate.Check].
	Check = cephstate.Check
	// Mons is an alias for [cephstate.Mons].
	Mons = cephstate.Mons
	// Mgr is an alias for [cephstate.Mgr].
	Mgr = cephstate.Mgr
	// OSDs is an alias for [cephstate.OSDs].
	OSDs = cephstate.OSDs
	// PGState is an alias for [cephstate.PGState].
	PGState = cephstate.PGState
	// PGs is an alias for [cephstate.PGs].
	PGs = cephstate.PGs
	// IO is an alias for [cephstate.IO].
	IO = cephstate.IO
	// Status is an alias for [cephstate.Status].
	Status = cephstate.Status
	// State is an alias for [cephstate.State].
	State = cephstate.State
)

// Settings is the plugin's profile configuration.
type Settings struct {
	// Namespace pins where Rook runs. Empty derives it from the tools workload.
	Namespace string
	// ToolsSelector pins the tools pod's label selector. Empty discovers the pod
	// by deployment name instead.
	ToolsSelector string
}

// Defaults leave everything to discovery, following the pattern the earlier
// plugins arrived at the hard way: every hardcoded name and namespace in this
// codebase was wrong on the first real cluster it met.
func Defaults() Settings { return Settings{} }

// SettingsFrom reads a profile's plugin block over the defaults.
func SettingsFrom(raw map[string]any) Settings {
	s := Defaults()
	if v, ok := raw["namespace"].(string); ok && v != "" {
		s.Namespace = v
	}
	if v, ok := raw["tools_selector"].(string); ok && v != "" {
		s.ToolsSelector = v
	}
	return s
}

// toolsSuffix is what a Rook tools Deployment's name ends with.
const toolsSuffix = "tools"

// Plugin observes Ceph.
type Plugin struct {
	client   *kube.Client
	settings Settings

	namespace  string
	tier       kube.Tier
	tierReason string
}

// New builds the plugin.
func New(c *kube.Client, settings Settings) *Plugin {
	return &Plugin{client: c, settings: settings}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return Name }

// Detect finds Rook's tools workload and decides the tier.
//
// The tools Deployment is the probe rather than the CephCluster CRD, because the
// CRD being registered does not mean a cluster exists, and the tools pod is what
// the detail actually comes from. Its namespace is discovered from the workload
// rather than assumed.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	ns, err := p.findNamespace(ctx)
	if err != nil || ns == "" {
		p.tier = kube.TierAbsent
		return false, err
	}
	p.namespace = ns

	pod, err := p.toolsPod(ctx)
	if err != nil {
		// Rook is installed but the tools deployment is scaled to zero, which is a
		// common way to run it. Ceph is present; the detail is not reachable.
		p.tier = kube.TierInformer
		p.tierReason = "no running tools pod: " + err.Error()
		// Present, not reachable: reported so --debug-snapshot can explain the
		// thin pane, and active so the pane still exists. See plugin.Source.
		return true, err
	}
	ok, forbidden := p.client.ExecProbe(ctx, ns, pod, "")
	switch {
	case ok:
		p.tier = kube.TierFull
	case forbidden:
		p.tier = kube.TierInformer
		p.tierReason = "no pods/exec permission on " + ns + " — use --server for full detail"
	default:
		// Not a verdict — poll re-derives this every time.
		p.tier = kube.TierInformer
		p.tierReason = "tools pod " + pod + " did not answer"
	}
	return true, nil
}

// findNamespace locates the namespace holding Rook's tools Deployment.
func (p *Plugin) findNamespace(ctx context.Context) (string, error) {
	if p.settings.Namespace != "" {
		return p.settings.Namespace, nil
	}
	list, err := p.client.Typed.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list deployments: %w", err)
	}
	for _, d := range list.Items {
		// "rook-ceph-tools" is the conventional name; matching both markers avoids
		// adopting an unrelated "tools" deployment in a shared namespace.
		if strings.HasSuffix(d.Name, toolsSuffix) && strings.Contains(d.Name, "ceph") {
			return d.Namespace, nil
		}
	}
	return "", nil
}

// toolsPod finds a Running tools pod.
//
// A configured selector is honored; otherwise pods are matched by name against
// the tools Deployment, since a Deployment's pods carry its name as a prefix and
// Rook's labels have varied across releases.
func (p *Plugin) toolsPods(ctx context.Context) ([]string, error) {
	if p.settings.ToolsSelector != "" {
		return p.client.PodCandidates(ctx, p.namespace, p.settings.ToolsSelector, nil)
	}
	// No selector: match by name, but through PodCandidates so the readiness and
	// terminating-pod rules apply here too. A tools pod on a node that has just
	// gone down still reports phase Running, and picking it costs the pane its
	// detail for as long as the pod object survives.
	return p.client.PodCandidates(ctx, p.namespace, "", func(pod *corev1.Pod) bool {
		return strings.Contains(pod.Name, toolsSuffix)
	})
}

// toolsPod returns the best tools pod, for callers that only need one.
func (p *Plugin) toolsPod(ctx context.Context) (string, error) {
	pods, err := p.toolsPods(ctx)
	if err != nil {
		return "", err
	}
	return pods[0], nil
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
	// Attempted every poll, whatever happened last time — including a permission
	// denial, which can be repaired mid-session. See [kube.Forbidden].
	pods, err := p.toolsPods(ctx)
	if err != nil {
		state.Tier = kube.TierInformer
		state.TierReason = "no tools pod ready right now"
		return state
	}

	out, pod, err := p.client.ExecFirstOf(ctx, p.namespace, pods, "",
		[]string{"ceph", "-s", "--format=json"})
	if err != nil {
		state.Tier = kube.TierInformer
		if kube.Forbidden(err) {
			state.TierReason = "no pods/exec permission on " + p.namespace + " — use --server for full detail"
		} else {
			state.TierReason = fmt.Sprintf("no tools pod answered (tried %d)", len(pods))
		}
		return state
	}
	state.Pod = pod
	// A pod answered, so whatever detection concluded is out of date.
	p.tier, p.tierReason = kube.TierFull, ""
	state.Tier, state.TierReason = kube.TierFull, ""
	status, err := ParseStatus([]byte(out))
	if err != nil {
		state.Err = err
		return state
	}

	// Fill in the active manager's name when the summary omitted it. Best-effort:
	// a failure leaves ActiveUnknown true, which the pane renders as "name not
	// reported" — still better than a false "no active manager".
	if status.Mgr.ActiveUnknown() {
		if name, err := p.activeMgrName(ctx, pod); err == nil && name != "" {
			status.Mgr.Active = name
		}
	}

	state.Status = status
	return state
}

// activeMgrName reads the active manager from `ceph mgr stat -f json`.
//
// A second exec is unwelcome, but current releases emit a summary-only mgrmap with
// no active_name and this is the canonical small endpoint that has it. It is only
// called when the name is actually missing, so a release that reports it pays
// nothing.
func (p *Plugin) activeMgrName(ctx context.Context, pod string) (string, error) {
	out, err := p.client.Exec(ctx, p.namespace, pod, "", []string{"ceph", "mgr", "stat", "-f", "json"})
	if err != nil {
		return "", err
	}
	return ParseMgrStat([]byte(out))
}

// Cells implements plugin.BannerProvider.
func (p *Plugin) Cells(s *store.Store) []health.Cell {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		return nil
	}

	cell := health.Cell{Name: "Ceph"}
	st := state.Status
	switch {
	case state.Err != nil:
		cell.Status = health.StatusWarn
		cell.Detail = "read error"
	case state.Tier != kube.TierFull:
		cell.Status = health.StatusLoading
		cell.Detail = "no detail"
	case st.Health == "HEALTH_ERR":
		cell.Status = health.StatusErr
		cell.Detail = firstCheckName(st.Checks)
	case st.Health == "HEALTH_WARN":
		cell.Status = health.StatusWarn
		cell.Detail = firstCheckName(st.Checks)
	case !st.OSDs.Healthy():
		// Ceph can report OK while an OSD is out, if the data is still replicated.
		cell.Status = health.StatusWarn
		cell.Detail = fmt.Sprintf("%d/%d OSDs up", st.OSDs.Up, st.OSDs.Total)
	case st.HealthOK():
		cell.Status = health.StatusOK
	default:
		cell.Status = health.StatusLoading
	}
	return []health.Cell{cell}
}

// firstCheckName names the check driving the status, so the cell says what is
// wrong rather than only that something is.
func firstCheckName(checks []Check) string {
	if len(checks) == 0 {
		return ""
	}
	return checks[0].Name
}


