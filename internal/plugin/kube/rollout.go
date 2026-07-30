package kube

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Rollout is one workload's progress toward its own current pod template.
//
// Every plugin here watches something that gets upgraded, and none of them could
// previously say whether an upgrade had finished. Readiness does not answer it: a
// DaemonSet whose spec was updated an hour ago reports every pod Ready while half
// of them still run the old image. Ready is about now; updated is about which
// version is running.
//
// # The state nothing else reports
//
// A workload carrying instances cannot be restarted at the controller's
// convenience. Open vSwitch holds every VM's networking on its host, so
// restarting its pod cuts those instances off until it returns. Charts that know
// this ship the DaemonSet with the OnDelete update strategy: the spec updates and
// Kubernetes then does nothing whatsoever until an operator drains each host and
// deletes its pod by hand.
//
// That state is invisible to every standard tool. `kubectl rollout status` refuses
// outright — "rollout status is only available for RollingUpdate strategy type" —
// and the workload reports no condition, no event and no complaint. It sits at 3
// of 57 updated, indefinitely, looking exactly like a healthy DaemonSet. A chart
// bumped and never finished looks identical on day one and week six.
//
// So [Rollout.Manual] is its own state rather than a flavor of "rolling". A
// rolling workload will be fine if left alone. A manual one will not.
type Rollout struct {
	// Desired, Updated and Ready are pod counts. Updated is the number running
	// the workload's current pod template, which the controllers maintain for
	// every update strategy — including OnDelete, where nothing else reports
	// progress at all.
	Desired, Updated, Ready int32
	// Manual reports that no controller will finish this on its own.
	Manual bool
	// StaleNodes names the nodes still running a superseded pod, sorted. Only
	// node-pinned workloads populate it; see [Client.StaleNodes].
	StaleNodes []string
}

// Stale is how many pods are still running a superseded template.
func (r Rollout) Stale() int32 { return max(r.Desired-r.Updated, 0) }

// Converged reports whether every pod runs the current template.
//
// A workload with no desired pods is converged: there is nothing left to update.
// That is the honest answer for the empty DaemonSets a per-node-configuration
// split leaves behind when a node is relabeled.
func (r Rollout) Converged() bool { return r.Stale() == 0 }

// Known reports whether this rollout describes anything at all, so a caller can
// tell "nothing to update" from "never looked".
func (r Rollout) Known() bool { return r.Desired > 0 }

// Add folds another workload's rollout into this one.
//
// Aggregation is needed because a single logical component is often several
// workloads. OpenStack-Helm splits one service across a DaemonSet per distinct
// node configuration, naming each by a hash of it — a cluster can carry four
// `openvswitch-server-*` DaemonSets, two of them empty leftovers from nodes since
// relabeled. Reporting those as four rows fragments the one number an operator
// wants and pads it with meaningless zeroes.
//
// Manual is true only when *every* contributing workload is manual. A component
// served by a mix has a half that the controller is already replacing, and calling
// the whole thing manual would send someone hunting for pods to delete that are
// being deleted for them.
func (r Rollout) Add(other Rollout) Rollout {
	if !r.Known() {
		return other
	}
	if !other.Known() {
		return r
	}
	return Rollout{
		Desired:    r.Desired + other.Desired,
		Updated:    r.Updated + other.Updated,
		Ready:      r.Ready + other.Ready,
		Manual:     r.Manual && other.Manual,
		StaleNodes: append(append([]string(nil), r.StaleNodes...), other.StaleNodes...),
	}
}

// RolloutOfDeployment reads a Deployment's rollout state.
func RolloutOfDeployment(d *appsv1.Deployment) Rollout {
	return Rollout{
		Desired: d.Status.Replicas,
		Updated: d.Status.UpdatedReplicas,
		Ready:   d.Status.ReadyReplicas,
	}
}

// RolloutOfStatefulSet reads a StatefulSet's rollout state.
func RolloutOfStatefulSet(s *appsv1.StatefulSet) Rollout {
	return Rollout{
		Desired: s.Status.Replicas,
		Updated: s.Status.UpdatedReplicas,
		Ready:   s.Status.ReadyReplicas,
		Manual:  s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType,
	}
}

// RolloutOfDaemonSet reads a DaemonSet's rollout state.
func RolloutOfDaemonSet(d *appsv1.DaemonSet) Rollout {
	return Rollout{
		Desired: d.Status.DesiredNumberScheduled,
		Updated: d.Status.UpdatedNumberScheduled,
		Ready:   d.Status.NumberReady,
		Manual:  d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType,
	}
}

// StaleNodes names the nodes whose DaemonSet pod is still on a superseded
// revision.
//
// Only DaemonSets resolve to nodes, and that is a judgment rather than a
// shortcut. A DaemonSet pod's node is the unit of work — finishing a manual
// rollout means draining that host and deleting that pod — whereas a Deployment
// pod's node was chosen by the scheduler and naming it tells nobody anything they
// can act on.
//
// The current revision is the highest-numbered ControllerRevision the DaemonSet
// owns, and a pod's controller-revision-hash label is the suffix of that
// revision's name. That is the same comparison the DaemonSet controller makes to
// compute UpdatedNumberScheduled, so these names cannot disagree with the count
// they appear beside.
//
// Errors yield no names rather than an error: this is the detail under a count
// that is already established, and losing the names is far better than losing the
// number.
func (c *Client) StaleNodes(ctx context.Context, ds *appsv1.DaemonSet) []string {
	current := c.currentRevision(ctx, ds)
	if current == "" {
		return nil
	}
	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return nil
	}
	pods, err := c.Typed.CoreV1().Pods(ds.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil
	}

	var out []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || !ownedByName(pod.OwnerReferences, ds.Name) {
			continue
		}
		if pod.Labels["controller-revision-hash"] != current {
			out = append(out, pod.Spec.NodeName)
		}
	}
	sort.Strings(out)
	return out
}

// currentRevision returns the pod-template hash a DaemonSet's up-to-date pods
// carry, or "" when it cannot be determined.
func (c *Client) currentRevision(ctx context.Context, ds *appsv1.DaemonSet) string {
	list, err := c.Typed.AppsV1().ControllerRevisions(ds.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}

	best, name := int64(-1), ""
	for i := range list.Items {
		rev := &list.Items[i]
		if ownedByName(rev.OwnerReferences, ds.Name) && rev.Revision > best {
			best, name = rev.Revision, rev.Name
		}
	}
	if name == "" {
		return ""
	}
	// The pod label carries only the hash, which is the revision name's last
	// dash-separated field. Split from the right, because the prefix is the
	// DaemonSet's own name and that may contain dashes of its own.
	if i := strings.LastIndex(name, "-"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func ownedByName(refs []metav1.OwnerReference, name string) bool {
	for _, ref := range refs {
		if ref.Name == name {
			return true
		}
	}
	return false
}

// ShortNodeNames trims each name to its first DNS label and caps the list,
// reporting how many were dropped.
//
// Node names are FQDNs sharing a domain, so the suffix is identical on every
// entry and pure noise in a one-line summary. The cap exists because this renders
// inline: fifty-four names would push everything else off the pane, and the count
// beside them already carries the magnitude.
func ShortNodeNames(names []string, limit int) (shown []string, more int) {
	shown = make([]string, 0, min(len(names), limit))
	for i, n := range names {
		if i == limit {
			return shown, len(names) - limit
		}
		if dot := strings.Index(n, "."); dot > 0 {
			n = n[:dot]
		}
		shown = append(shown, n)
	}
	return shown, 0
}
