package kube

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/binnacle/pkg/subsystem"
)

// Rollout is one workload's progress toward its own current pod template.
// An alias for [subsystem.Rollout].
type Rollout = subsystem.Rollout

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
