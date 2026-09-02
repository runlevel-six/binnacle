package openstack

import (
	"sort"
	"strings"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// This file answers the question the cloud's own APIs cannot: is each OpenStack
// service running the version its charts ask for?
//
// # Why the API cannot answer it
//
// Neither Nova's `os-services` nor Neutron's agent list carries a version field,
// so "what version is this service" is not a question the cloud can be asked.
// Worse, only three projects have an agent registry at all — compute, network and
// block storage — which is why the agent table shows three rows on a cloud running
// fifteen services. Keystone, Glance, Heat, Octavia, Barbican, Placement and Magnum
// are structurally invisible to it: they have no agents to report.
//
// They are all plainly visible as Kubernetes workloads. So this reads the cluster
// instead, and in doing so answers both questions with one source — which services
// exist, and whether each has finished rolling.
//
// # Read from the store, not from the API server
//
// The core workload watcher already informs on every Deployment, StatefulSet and
// DaemonSet in the cluster; on a production cloud that snapshot holds around 250
// objects and is kept warm regardless. Reading it costs nothing and, more
// importantly, needs no Kubernetes client of its own — which is what lets this
// plugin stay what its package comment says it is: a thing that authenticates to a
// cloud, with no exec tier and no cluster credentials.
//
// That is not tidiness. Detection would otherwise have two prerequisites that can
// each fail independently, and the plugin would go absent for want of a
// `clouds.yaml` on exactly the cluster where an operator has Kubernetes access and
// no cloud admin credentials — the person most likely to be watching an OpenStack
// rollout.

// Service is one OpenStack service's rollout progress, summed across every
// workload that serves it.
//
// Keyed on the service rather than the workload because "is Nova up to date" is
// the question; Nova is nine Deployments and a DaemonSet, and listing them
// separately turns one answer into ten rows. When a service is behind, the
// component that is behind gets named — see [Service.Behind].
type Service struct {
	// Name is the service, from the `application` label: "nova", "keystone".
	Name string
	kube.Rollout
	// Components is the per-workload breakdown, only for the ones still behind.
	// The service name says what is stuck; this says which part of it.
	Components []Component
}

// Component is one workload family within a service, e.g. "nova-compute".
type Component struct {
	Name string
	kube.Rollout
}

// Services is what the version view renders.
type Services struct {
	// Namespace is where the OpenStack workloads were found.
	Namespace string
	// Items is every service, converged or not, ordered with the ones needing
	// attention first. See [sortServices].
	Items []Service
}

// Pending returns the services with pods still to update, in Items order.
func (s Services) Pending() []Service {
	var out []Service
	for _, svc := range s.Items {
		if !svc.Converged() {
			out = append(out, svc)
		}
	}
	return out
}

// Converged reports whether every service has finished rolling. An empty set
// answers false: before the snapshot arrives the honest answer is that we do not
// know, and reporting a finished rollout we have not observed is the one wrong
// answer that reads as good news.
func (s Services) Converged() bool {
	return len(s.Items) > 0 && len(s.Pending()) == 0
}

// StalePods totals the pods across every service still to update.
func (s Services) StalePods() int32 {
	var n int32
	for _, svc := range s.Items {
		n += svc.Stale()
	}
	return n
}

// NeedsOperator reports whether any pending service will never finish on its own.
func (s Services) NeedsOperator() bool {
	for _, svc := range s.Items {
		if !svc.Converged() && svc.Manual {
			return true
		}
	}
	return false
}

// Behind names the components of a service that are still rolling, most-behind
// first, for the note beside its row.
func (s Service) Behind() []Component {
	var out []Component
	for _, c := range s.Components {
		if !c.Converged() {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Stale() > out[j].Stale() })
	return out
}

// networkOwned are services reported by the network frame instead of here.
//
// They run in the OpenStack namespace and are OpenStack's by any reasonable
// reading, so excluding them is a presentation decision rather than a taxonomic
// one: OVN and Open vSwitch are the switching layer, the network frame reports
// them component by component with the nodes still to drain, and listing them
// here as well would say the same thing twice in two different groupings.
//
// The duplication was not merely untidy. Both frames contribute a banner cell, so
// a half-finished OVS rollout lit two cells with the same pod count and an
// operator had no way to tell whether that was one problem or two.
var networkOwned = map[string]bool{"ovn": true, "openvswitch": true}

// serviceMarkers are projects only an OpenStack control plane runs, used to pick
// the namespace out of a cluster-wide snapshot.
//
// Several rather than one, because a deployment may legitimately omit any given
// service — a cloud with no Cinder is a normal cloud — and matching on an
// intersection survives that where a single name would not.
var serviceMarkers = map[string]bool{
	"keystone": true, "nova": true, "neutron": true,
	"glance": true, "placement": true, "cinder": true,
}

// CollectServices aggregates the OpenStack workloads out of the cluster-wide
// snapshot.
//
// namespace pins where to look; empty derives it. Derived rather than defaulted
// to "openstack" on the same reasoning as every other discovery here: hardcoded
// namespaces in this codebase have each been wrong on the first real cluster they
// met.
func CollectServices(s *store.Store, namespace string) (Services, bool) {
	snap, ok := store.Get[model.Snapshot[model.Workload]](s, model.KeyWorkloadWorkloads)
	if !ok {
		return Services{}, false
	}

	ns := namespace
	if ns == "" {
		ns = openStackNamespace(snap.Items)
	}
	if ns == "" {
		return Services{}, false
	}

	byService := map[string]*Service{}
	byComponent := map[string]map[string]*Component{}
	for _, w := range snap.Items {
		if w.Namespace != ns {
			continue
		}
		app := w.Labels["application"]
		if app == "" {
			// No chart labels. Skipped rather than keyed by name, because this
			// view is a list of *services* and a supporting workload with no
			// service label would appear as a service that does not exist. The
			// namespace-wide totals are not the job here.
			continue
		}
		if networkOwned[app] {
			continue
		}
		if w.Desired == 0 {
			// Not an idle service — the empty leftovers a per-node-configuration
			// split leaves behind when a node is relabeled or removed. Counting
			// them adds rows that can never say anything.
			continue
		}

		r := kube.Rollout{
			Desired: w.Desired, Updated: w.Updated, Ready: w.Ready, Manual: w.Manual,
		}
		svc, seen := byService[app]
		if !seen {
			svc = &Service{Name: app}
			byService[app] = svc
			byComponent[app] = map[string]*Component{}
		}
		svc.Rollout = svc.Add(r)

		name := app
		if part := w.Labels["component"]; part != "" {
			name = app + "-" + part
		}
		c, seenC := byComponent[app][name]
		if !seenC {
			c = &Component{Name: name}
			byComponent[app][name] = c
		}
		c.Rollout = c.Add(r)
	}

	out := Services{Namespace: ns, Items: make([]Service, 0, len(byService))}
	for app, svc := range byService {
		for _, c := range byComponent[app] {
			if !c.Converged() {
				svc.Components = append(svc.Components, *c)
			}
		}
		sort.SliceStable(svc.Components, func(i, j int) bool {
			return svc.Components[i].Name < svc.Components[j].Name
		})
		out.Items = append(out.Items, *svc)
	}
	sortServices(out.Items)
	return out, true
}

// openStackNamespace picks the namespace running the most distinct OpenStack
// services.
//
// Counting distinct services rather than raw workloads, so a namespace holding one
// stray object labeled `application=nova` cannot outrank the one running the
// actual control plane.
func openStackNamespace(items []model.Workload) string {
	found := map[string]map[string]bool{}
	for _, w := range items {
		app := w.Labels["application"]
		if !serviceMarkers[app] {
			continue
		}
		if found[w.Namespace] == nil {
			found[w.Namespace] = map[string]bool{}
		}
		found[w.Namespace][app] = true
	}

	best, bestN := "", 0
	for ns, apps := range found {
		// Ties break on the name so the choice is stable across polls. A
		// dashboard that silently changed which namespace it was describing
		// would be worse than one that consistently picked the wrong one.
		if len(apps) > bestN || (len(apps) == bestN && ns < best) {
			best, bestN = ns, len(apps)
		}
	}
	return best
}

// sortServices orders for reading: the ones needing a human first, then the rest
// of the pending ones by how much is left, then everything converged by name.
func sortServices(items []Service) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Converged() != b.Converged() {
			return !a.Converged()
		}
		if !a.Converged() {
			if a.Manual != b.Manual {
				return a.Manual
			}
			if a.Stale() != b.Stale() {
				return a.Stale() > b.Stale()
			}
		}
		return a.Name < b.Name
	})
}

// TrimComponent shortens a component name by dropping its service prefix, since
// the service is already named in the row it sits beside.
func TrimComponent(service, component string) string {
	return strings.TrimPrefix(component, service+"-")
}
