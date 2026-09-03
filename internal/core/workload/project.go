package workload

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
)

// Workload kinds, as reported in model.Workload.Kind.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// ProjectNodes projects Nodes, deriving each node's role through the profile and
// attributing pod resource requests to the node they are scheduled on.
//
// pods may be nil, in which case the resource columns are left zero. They are
// filled by joining here rather than in a pane so that every consumer sees the
// same numbers.
func ProjectNodes(raw []*corev1.Node, pods []*corev1.Pod, roles profile.NodeRoles) model.Snapshot[model.Node] {
	usage := podResourceUsageByNode(pods)

	out := make([]model.Node, 0, len(raw))
	for _, n := range raw {
		node := ProjectNode(n, roles)
		if u, ok := usage[n.Name]; ok {
			node.RequestedCPU = u.cpuReq
			node.RequestedMemory = u.memReq
			node.LimitsCPU = u.cpuLim
			node.LimitsMemory = u.memLim
		}
		out = append(out, node)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return snapshot(out)
}

// ProjectNode projects one Node.
func ProjectNode(n *corev1.Node, roles profile.NodeRoles) model.Node {
	out := model.Node{
		Name:             n.Name,
		Status:           nodeReadyStatus(n),
		Roles:            roles.RolesOf(n.Labels),
		Role:             roles.RoleOf(n.Labels),
		Age:              time.Since(n.CreationTimestamp.Time),
		Version:          n.Status.NodeInfo.KubeletVersion,
		OSImage:          n.Status.NodeInfo.OSImage,
		KernelVersion:    n.Status.NodeInfo.KernelVersion,
		ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
		Cordoned:         n.Spec.Unschedulable,
		Labels:           n.Labels,
	}
	if cpu, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
		out.AllocatableCPU = cpu.MilliValue()
	}
	if mem, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
		out.AllocatableMemory = mem.Value()
	}

	// Pressure conditions. The kubelet sets these True when the node is in
	// resource trouble; counting them across nodes is how a pattern becomes
	// visible.
	for _, c := range n.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case corev1.NodeMemoryPressure:
			out.MemoryPressure = true
		case corev1.NodeDiskPressure:
			out.DiskPressure = true
		case corev1.NodePIDPressure:
			out.PIDPressure = true
		case corev1.NodeNetworkUnavailable:
			out.NetworkUnavail = true
		}
	}

	for _, addr := range n.Status.Addresses {
		switch addr.Type {
		case corev1.NodeInternalIP:
			if out.InternalIP == "" {
				out.InternalIP = addr.Address
			}
		case corev1.NodeExternalIP:
			if out.ExternalIP == "" {
				out.ExternalIP = addr.Address
			}
		}
	}
	return out
}

// nodeReadyStatus collapses node conditions into the status `kubectl get nodes`
// shows. A node with no Ready condition at all reports Unknown, which is what
// the kubelet's silence actually means.
func nodeReadyStatus(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			switch c.Status {
			case corev1.ConditionTrue:
				return "Ready"
			case corev1.ConditionFalse:
				return "NotReady"
			default:
				return "Unknown"
			}
		}
	}
	return "Unknown"
}

// resourceUsage is the summed requests and limits of the pods on one node.
type resourceUsage struct {
	cpuReq, memReq, cpuLim, memLim int64
}

// podResourceUsageByNode sums pod requests and limits per node, the way
// `kubectl describe node` and kube-capacity report them.
//
// Only pods that still hold a scheduling claim are counted: a Succeeded or
// Failed pod has released its resources, so including it would overstate
// utilization, sometimes wildly on a node that has run many jobs.
//
// Init containers are accounted as a maximum rather than a sum, matching the
// scheduler: they run sequentially, so a pod's effective init requirement is its
// largest init container, and the pod's request is the greater of that and the
// sum across its regular containers.
func podResourceUsageByNode(pods []*corev1.Pod) map[string]resourceUsage {
	out := make(map[string]resourceUsage)
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}

		var cpuReq, memReq, cpuLim, memLim int64
		for _, c := range p.Spec.Containers {
			cpuReq += c.Resources.Requests.Cpu().MilliValue()
			memReq += c.Resources.Requests.Memory().Value()
			cpuLim += c.Resources.Limits.Cpu().MilliValue()
			memLim += c.Resources.Limits.Memory().Value()
		}
		for _, c := range p.Spec.InitContainers {
			cpuReq = max(cpuReq, c.Resources.Requests.Cpu().MilliValue())
			memReq = max(memReq, c.Resources.Requests.Memory().Value())
			cpuLim = max(cpuLim, c.Resources.Limits.Cpu().MilliValue())
			memLim = max(memLim, c.Resources.Limits.Memory().Value())
		}

		u := out[p.Spec.NodeName]
		u.cpuReq += cpuReq
		u.memReq += memReq
		u.cpuLim += cpuLim
		u.memLim += memLim
		out[p.Spec.NodeName] = u
	}
	return out
}

// ProjectPods projects Pods, sorted by namespace then name.
func ProjectPods(raw []*corev1.Pod) model.Snapshot[model.Pod] {
	out := make([]model.Pod, 0, len(raw))
	for _, p := range raw {
		out = append(out, ProjectPod(p))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return snapshot(out)
}

// ProjectPod projects one Pod, reproducing the columns `kubectl get pods -o wide`
// shows.
func ProjectPod(p *corev1.Pod) model.Pod {
	out := model.Pod{
		Namespace: p.Namespace,
		Name:      p.Name,
		Age:       time.Since(p.CreationTimestamp.Time),
		IP:        p.Status.PodIP,
		Node:      p.Spec.NodeName,
		Phase:     string(p.Status.Phase),
	}

	// Sidecars count as containers, because that is what they are. A native
	// sidecar is declared as an init container with restartPolicy Always
	// (Kubernetes 1.29+), and it runs for the life of the pod rather than
	// completing. Left out of these totals, a pod with one reads 3/3 where
	// kubectl reads 4/4 — and worse, see podStatus: it never finishes
	// initializing, so the pod is permanently unhealthy.
	sidecars := restartableInitContainers(p)

	var ready, restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if !sidecars[cs.Name] {
			continue
		}
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	out.ReadyReady = ready
	out.ReadyTotal = int32(len(p.Spec.Containers) + len(sidecars))
	out.Restarts = restarts
	out.Status = podStatus(p)

	// Healthy means every container ready and running, or the pod ran to
	// completion. Everything else belongs in an unhealthy list.
	out.IsHealthy = (out.Status == "Running" && out.ReadyTotal > 0 && ready == out.ReadyTotal) ||
		out.Status == "Completed"
	return out
}

// podStatus derives the STATUS column, following the same precedence kubectl's
// printer uses: deletion, then init containers, then container states, then the
// pod phase.
func podStatus(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		if reason := terminatingReason(p); reason != "" {
			return reason
		}
		return "Terminating"
	}

	// An init container that has not completed successfully surfaces as
	// "Init:<reason>" or "Init:N/M".
	sidecars := restartableInitContainers(p)
	for i, ic := range p.Status.InitContainerStatuses {
		switch {
		case ic.State.Terminated != nil && ic.State.Terminated.ExitCode == 0:
			continue
		// A started sidecar is not unfinished initialization, it is a running
		// container. It never terminates, so without this it holds the pod at
		// "Init:1/2" for as long as the pod lives — which was reported as an
		// unhealthy pod, forever, on every cluster running one. Three dmzproxy
		// replicas and four virt-launchers on one management cluster read as
		// failing for twenty-seven and eighty-four days respectively, and a
		// signal that is never clear is a signal nobody reads.
		case sidecars[ic.Name] && ic.Started != nil && *ic.Started:
			continue
		case ic.State.Terminated != nil && ic.State.Terminated.Reason != "":
			return "Init:" + ic.State.Terminated.Reason
		case ic.State.Waiting != nil && ic.State.Waiting.Reason != "" && ic.State.Waiting.Reason != "PodInitializing":
			return "Init:" + ic.State.Waiting.Reason
		default:
			return fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
	}

	// A container-level state is more specific than the phase.
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}

	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return "Completed"
	case corev1.PodFailed:
		return "Error"
	default:
		return string(p.Status.Phase)
	}
}

// restartableInitContainers names the pod's native sidecars.
//
// A sidecar is an init container with restartPolicy Always: it starts in init
// order and then keeps running, so it is an init container by declaration and
// a regular container by behavior. Both the READY column and the init-progress
// status have to treat it as the latter, which is what kubectl does and what
// this reproduces.
func restartableInitContainers(p *corev1.Pod) map[string]bool {
	var out map[string]bool
	for _, ic := range p.Spec.InitContainers {
		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			if out == nil {
				out = make(map[string]bool, len(p.Spec.InitContainers))
			}
			out[ic.Name] = true
		}
	}
	return out
}

// terminatingReason picks the most informative reason from a pod being deleted,
// so a pod stuck terminating because of a crash still shows why.
func terminatingReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

// ProjectDeployments, ProjectStatefulSets and ProjectDaemonSets normalise the
// three workload kinds into one list so a pane renders a single section.

// ProjectDeployments projects apps/v1 Deployments.
func ProjectDeployments(raw []*appsv1.Deployment) []model.Workload {
	out := make([]model.Workload, 0, len(raw))
	for _, d := range raw {
		desired := int32(1) // a Deployment with no replicas set defaults to 1
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		out = append(out, model.Workload{
			Namespace: d.Namespace, Name: d.Name, Kind: KindDeployment,
			Ready: d.Status.ReadyReplicas, Desired: desired,
			Updated: d.Status.UpdatedReplicas,
			Labels:  d.Labels,
		})
	}
	return out
}

// ProjectStatefulSets projects apps/v1 StatefulSets.
func ProjectStatefulSets(raw []*appsv1.StatefulSet) []model.Workload {
	out := make([]model.Workload, 0, len(raw))
	for _, s := range raw {
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		out = append(out, model.Workload{
			Namespace: s.Namespace, Name: s.Name, Kind: KindStatefulSet,
			Ready: s.Status.ReadyReplicas, Desired: desired,
			Updated: s.Status.UpdatedReplicas,
			Manual:  s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType,
			Labels:  s.Labels,
		})
	}
	return out
}

// ProjectDaemonSets projects apps/v1 DaemonSets. Desired comes from the
// scheduler's own count rather than a spec field, since a DaemonSet's size is a
// function of how many nodes match it.
func ProjectDaemonSets(raw []*appsv1.DaemonSet) []model.Workload {
	out := make([]model.Workload, 0, len(raw))
	for _, d := range raw {
		out = append(out, model.Workload{
			Namespace: d.Namespace, Name: d.Name, Kind: KindDaemonSet,
			Ready: d.Status.NumberReady, Desired: d.Status.DesiredNumberScheduled,
			Updated: d.Status.UpdatedNumberScheduled,
			Manual:  d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType,
			Labels:  d.Labels,
		})
	}
	return out
}

// SortWorkloads orders workloads by kind, namespace, then name so the combined
// list is stable across renders.
func SortWorkloads(ws []model.Workload) {
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// FilterEvents keeps only the events the profile considers interesting, newest
// first.
//
// The filter exists for volume, not relevance: a busy cluster emits hundreds of
// events per second and an unfiltered pane is unreadable.
func FilterEvents(raw []*corev1.Event, filter profile.Events, project func(*corev1.Event) model.Event) []model.Event {
	out := make([]model.Event, 0, len(raw))
	for _, e := range raw {
		if !filter.Interesting(e.Namespace) {
			continue
		}
		out = append(out, project(e))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastTimestamp.After(out[j].LastTimestamp)
	})
	return out
}

func snapshot[T any](items []T) model.Snapshot[T] {
	return model.Snapshot[T]{Items: items, UpdatedAt: time.Now()}
}
