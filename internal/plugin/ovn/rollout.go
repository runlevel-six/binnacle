package ovn

import (
	ovnstate "github.com/runlevel-six/sextant/pkg/subsystem/ovn"

	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
)

// Component is one OVN or Open vSwitch workload family's rollout state. It
// lives in pkg/subsystem/ovn so a consumer outside this module can read it.
type Component = ovnstate.Component

// components are the workload families this plugin reports, in the order an
// upgrade must proceed through them.
//
// The order is not cosmetic. OVN's supported upgrade sequence is databases, then
// northd, then the per-host controllers, and the failure mode worth seeing is skew
// *between* those stages — a northd talking to controllers a release behind. Listed
// in upgrade order, the pane shows where the wave has got to by where the numbers
// stop being whole.
//
// Open vSwitch comes last because it is the one that does not move on its own: it
// carries every instance's networking, so its chart ships OnDelete and each host
// waits for someone to drain it. See [kube.Rollout].
var components = []struct {
	name string
	// match reports whether a workload of the given kind and name belongs to
	// this family. Matched on name rather than on chart labels, because this
	// plugin already discovers its databases by name suffix and a deployment
	// that renames its charts should not silently lose a row.
	match func(kind, name string) bool
}{
	{"ovsdb-nb", suffixOf("ovn-ovsdb-nb")},
	{"ovsdb-sb", suffixOf("ovn-ovsdb-sb")},
	{"ovn-northd", suffixOf("ovn-northd")},
	{"ovn-controller", containsOf("ovn-controller")},
	{"openvswitch", containsOf("openvswitch-server")},
}

func suffixOf(want string) func(kind, name string) bool {
	return func(_, name string) bool { return strings.HasSuffix(name, want) }
}

// containsOf matches a hash- or variant-suffixed family name, e.g.
// "openvswitch-server-921a8880a66a31b6" or "ovn-controller-default".
func containsOf(want string) func(kind, name string) bool {
	return func(_, name string) bool { return strings.Contains(name, want) }
}

// pollComponents reads the rollout state of every OVN and OVS workload family.
//
// Three list calls in one namespace, no exec and no elevated permission — this is
// the tier of detail that survives when `pods/exec` is denied, which matters
// because a denied exec is exactly when the Raft detail above it goes missing and
// the pane would otherwise have nothing left to say.
func (p *Plugin) pollComponents(ctx context.Context) []Component {
	if p.client == nil || p.namespace == "" {
		return nil
	}

	acc := map[string]kube.Rollout{}
	seen := map[string]bool{}
	add := func(name string, r kube.Rollout) {
		acc[name] = acc[name].Add(r)
		seen[name] = true
	}

	if list, err := p.client.Typed.AppsV1().Deployments(p.namespace).
		List(ctx, metav1.ListOptions{}); err == nil {
		for i := range list.Items {
			d := &list.Items[i]
			if name, ok := familyOf("Deployment", d.Name); ok {
				add(name, kube.RolloutOfDeployment(d))
			}
		}
	}

	if list, err := p.client.Typed.AppsV1().StatefulSets(p.namespace).
		List(ctx, metav1.ListOptions{}); err == nil {
		for i := range list.Items {
			s := &list.Items[i]
			if name, ok := familyOf("StatefulSet", s.Name); ok {
				add(name, kube.RolloutOfStatefulSet(s))
			}
		}
	}

	if list, err := p.client.Typed.AppsV1().DaemonSets(p.namespace).
		List(ctx, metav1.ListOptions{}); err == nil {
		for i := range list.Items {
			ds := &list.Items[i]
			name, ok := familyOf("DaemonSet", ds.Name)
			if !ok {
				continue
			}
			r := kube.RolloutOfDaemonSet(ds)
			if !r.Converged() {
				// Only when behind, and only for DaemonSets: naming nodes costs
				// two more calls and the answer on a converged family is always
				// the empty list.
				r.StaleNodes = p.client.StaleNodes(ctx, ds)
			}
			add(name, r)
		}
	}

	out := make([]Component, 0, len(acc))
	for _, c := range components {
		if !seen[c.name] {
			// Absent, not broken. A deployment without a VPN agent or without a
			// separate northd should show no row rather than an empty one.
			continue
		}
		r := acc[c.name]
		sort.Strings(r.StaleNodes)
		out = append(out, Component{Name: c.name, Rollout: r})
	}
	return out
}

func familyOf(kind, name string) (string, bool) {
	for _, c := range components {
		if c.match(kind, name) {
			return c.name, true
		}
	}
	return "", false
}

// ComponentsConverged reports whether every family has finished rolling.
func ComponentsConverged(cs []Component) bool {
	for _, c := range cs {
		if !c.Converged() {
			return false
		}
	}
	return true
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
