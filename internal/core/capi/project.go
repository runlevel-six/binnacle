package capi

import (
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/runlevel-six/binnacle/pkg/model"
)

// Projections turn API objects into model snapshots. They are pure functions of
// their input, which is what makes them testable from unstructured literals
// without a cluster.

// ProjectClusters projects cluster.x-k8s.io Clusters.
func ProjectClusters(objs []*unstructured.Unstructured) model.Snapshot[model.Cluster] {
	out := make([]model.Cluster, 0, len(objs))
	for _, o := range objs {
		out = append(out, model.Cluster{
			Namespace:    o.GetNamespace(),
			Name:         o.GetName(),
			ClusterClass: uStr(o, "spec", "topology", "class"),
			Version:      uStr(o, "spec", "topology", "version"),
			Phase:        uStr(o, "status", "phase"),
			Available:    hasTrueCondition(o, "Available"),
			Paused:       pausedAnnotation(o),
			ControlPlane: replicaBucket(o, "controlPlane"),
			Workers:      replicaBucket(o, "workers"),
			Age:          uAge(o),
			Conditions:   conditions(o),
		})
	}
	sortByNamespacedName(out, func(c model.Cluster) (string, string) { return c.Namespace, c.Name })
	return snapshot(out)
}

func replicaBucket(o *unstructured.Unstructured, field string) model.ReplicaBucket {
	return model.ReplicaBucket{
		Desired:   uInt32(o, "status", field, "desiredReplicas"),
		Current:   uInt32(o, "status", field, "replicas"),
		Ready:     uInt32(o, "status", field, "readyReplicas"),
		Available: uInt32(o, "status", field, "availableReplicas"),
		UpToDate:  uInt32(o, "status", field, "upToDateReplicas"),
	}
}

// ProjectKCPs projects KubeadmControlPlanes.
func ProjectKCPs(objs []*unstructured.Unstructured) model.Snapshot[model.KubeadmControlPlane] {
	out := make([]model.KubeadmControlPlane, 0, len(objs))
	for _, o := range objs {
		out = append(out, model.KubeadmControlPlane{
			Namespace:           o.GetNamespace(),
			Name:                o.GetName(),
			ClusterName:         clusterNameLabel(o),
			Version:             uStr(o, "spec", "version"),
			DesiredReplicas:     uInt32(o, "spec", "replicas"),
			Replicas:            uInt32(o, "status", "replicas"),
			UpToDateReplicas:    upToDateReplicas(o),
			ReadyReplicas:       uInt32(o, "status", "readyReplicas"),
			AvailableReplicas:   uInt32(o, "status", "availableReplicas"),
			UnavailableReplicas: uInt32(o, "status", "unavailableReplicas"),
			Available:           hasTrueCondition(o, "Available"),
			Initialized:         uBool(o, "status", "initialized"),
			Paused:              pausedAnnotation(o),
			Age:                 uAge(o),
			Conditions:          conditions(o),
		})
	}
	sortByNamespacedName(out, func(k model.KubeadmControlPlane) (string, string) { return k.Namespace, k.Name })
	return snapshot(out)
}

// ProjectMachineDeployments projects MachineDeployments.
func ProjectMachineDeployments(objs []*unstructured.Unstructured) model.Snapshot[model.MachineDeployment] {
	out := make([]model.MachineDeployment, 0, len(objs))
	for _, o := range objs {
		out = append(out, model.MachineDeployment{
			Namespace:         o.GetNamespace(),
			Name:              o.GetName(),
			ClusterName:       clusterNameLabel(o),
			Phase:             uStr(o, "status", "phase"),
			Version:           uStr(o, "spec", "template", "spec", "version"),
			DesiredReplicas:   uInt32(o, "spec", "replicas"),
			Replicas:          uInt32(o, "status", "replicas"),
			UpToDateReplicas:  upToDateReplicas(o),
			ReadyReplicas:     uInt32(o, "status", "readyReplicas"),
			AvailableReplicas: uInt32(o, "status", "availableReplicas"),
			Available:         hasTrueCondition(o, "Available"),
			Paused:            pausedAnnotation(o),
			Age:               uAge(o),
			Conditions:        conditions(o),
		})
	}
	sortByNamespacedName(out, func(m model.MachineDeployment) (string, string) { return m.Namespace, m.Name })
	return snapshot(out)
}

// ProjectMachines projects Machines, including the provider-specific
// infrastructure reference so a Metal3Machine can be joined to it later.
func ProjectMachines(objs []*unstructured.Unstructured) model.Snapshot[model.Machine] {
	out := make([]model.Machine, 0, len(objs))
	for _, o := range objs {
		ownerKind, ownerName := firstOwnerOfKind(o, "KubeadmControlPlane", "MachineSet")
		out = append(out, model.Machine{
			Namespace:   o.GetNamespace(),
			Name:        o.GetName(),
			ClusterName: clusterNameLabel(o),
			NodeName:    uStr(o, "status", "nodeRef", "name"),
			Phase:       uStr(o, "status", "phase"),
			Version:     uStr(o, "spec", "version"),
			ProviderID:  uStr(o, "spec", "providerID"),
			OwnerKind:   ownerKind,
			OwnerName:   ownerName,
			InfraKind:   uStr(o, "spec", "infrastructureRef", "kind"),
			InfraName:   uStr(o, "spec", "infrastructureRef", "name"),
			Age:         uAge(o),
			Conditions:  conditions(o),
		})
	}
	sortByNamespacedName(out, func(m model.Machine) (string, string) { return m.Namespace, m.Name })
	return snapshot(out)
}

// ProjectMetal3Clusters projects Metal3Clusters.
func ProjectMetal3Clusters(objs []*unstructured.Unstructured) model.Snapshot[model.Metal3Cluster] {
	out := make([]model.Metal3Cluster, 0, len(objs))
	for _, o := range objs {
		// failureMessage is the human-readable form and failureReason a short
		// enum; prefer the message and fall back to the reason.
		errMsg := trimToOneLine(uStr(o, "status", "failureMessage"))
		if errMsg == "" {
			errMsg = uStr(o, "status", "failureReason")
		}
		out = append(out, model.Metal3Cluster{
			Namespace:    o.GetNamespace(),
			Name:         o.GetName(),
			ClusterName:  clusterNameLabel(o),
			Ready:        uBool(o, "status", "ready"),
			EndpointHost: uStr(o, "spec", "controlPlaneEndpoint", "host"),
			EndpointPort: uInt32(o, "spec", "controlPlaneEndpoint", "port"),
			ErrorMessage: errMsg,
			Age:          uAge(o),
			Conditions:   conditions(o),
		})
	}
	sortByNamespacedName(out, func(c model.Metal3Cluster) (string, string) { return c.Namespace, c.Name })
	return snapshot(out)
}

// ProjectMetal3Machines projects Metal3Machines.
func ProjectMetal3Machines(objs []*unstructured.Unstructured) model.Snapshot[model.Metal3Machine] {
	out := make([]model.Metal3Machine, 0, len(objs))
	for _, o := range objs {
		bmhNS, bmhName := bareMetalHostRef(o)
		out = append(out, model.Metal3Machine{
			Namespace:    o.GetNamespace(),
			Name:         o.GetName(),
			Ready:        uBool(o, "status", "ready"),
			ProviderID:   uStr(o, "spec", "providerID"),
			BMHNamespace: bmhNS,
			BMHName:      bmhName,
			Age:          uAge(o),
			Conditions:   conditions(o),
		})
	}
	sortByNamespacedName(out, func(m model.Metal3Machine) (string, string) { return m.Namespace, m.Name })
	return snapshot(out)
}

// bareMetalHostRef reads the consumed host from the metal3.io/BareMetalHost
// annotation, whose value is "namespace/name".
//
// An unqualified value is treated as a name in the Metal3Machine's own
// namespace, which is the only sensible reading and matches how Metal3 writes
// the annotation in the common case.
func bareMetalHostRef(o *unstructured.Unstructured) (namespace, name string) {
	v, ok := o.GetAnnotations()[annotationBareMetalHost]
	if !ok || v == "" {
		return "", ""
	}
	if ns, n, found := strings.Cut(v, "/"); found {
		return ns, n
	}
	return o.GetNamespace(), v
}

// ProjectBareMetalHosts projects BareMetalHosts.
func ProjectBareMetalHosts(objs []*unstructured.Unstructured) model.Snapshot[model.BareMetalHost] {
	out := make([]model.BareMetalHost, 0, len(objs))
	for _, o := range objs {
		out = append(out, model.BareMetalHost{
			Namespace:         o.GetNamespace(),
			Name:              o.GetName(),
			State:             uStr(o, "status", "provisioning", "state"),
			OperationalStatus: uStr(o, "status", "operationalStatus"),
			PoweredOn:         uBool(o, "status", "poweredOn"),
			Online:            uBool(o, "spec", "online"),
			ConsumerKind:      uStr(o, "spec", "consumerRef", "kind"),
			ConsumerNamespace: uStr(o, "spec", "consumerRef", "namespace"),
			ConsumerName:      uStr(o, "spec", "consumerRef", "name"),
			ErrorMessage:      trimToOneLine(uStr(o, "status", "errorMessage")),
			Age:               uAge(o),
			Labels:            o.GetLabels(),
		})
	}
	sortByNamespacedName(out, func(b model.BareMetalHost) (string, string) { return b.Namespace, b.Name })
	return snapshot(out)
}

// ProjectEvents projects core/v1 Events, newest first.
func ProjectEvents(raw []*corev1.Event) model.Snapshot[model.Event] {
	out := make([]model.Event, 0, len(raw))
	for _, e := range raw {
		out = append(out, ProjectEvent(e))
	}
	SortEventsNewestFirst(out)
	return snapshot(out)
}

// ProjectEvent projects one core/v1 Event.
func ProjectEvent(e *corev1.Event) model.Event {
	// Newer events populate EventTime instead of LastTimestamp, so fall back
	// rather than reporting a zero time that would sort to the end.
	last := e.LastTimestamp.Time
	if last.IsZero() {
		last = e.EventTime.Time
	}
	first := e.FirstTimestamp.Time
	if first.IsZero() {
		first = last
	}
	return model.Event{
		Namespace:      e.Namespace,
		Type:           e.Type,
		Reason:         e.Reason,
		ObjectKind:     e.InvolvedObject.Kind,
		ObjectName:     e.InvolvedObject.Name,
		Message:        trimToOneLine(e.Message),
		FirstTimestamp: first,
		LastTimestamp:  last,
		Count:          e.Count,
	}
}

// SortEventsNewestFirst orders events the way `kubectl get events` does, which
// is what a reader glancing at the pane expects.
func SortEventsNewestFirst(events []model.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].LastTimestamp.After(events[j].LastTimestamp)
	})
}

func snapshot[T any](items []T) model.Snapshot[T] {
	return model.Snapshot[T]{Items: items, UpdatedAt: time.Now()}
}

// sortByNamespacedName keeps every snapshot in a stable order so tables do not
// shuffle between renders. Namespace is included because watching all namespaces
// means names are no longer unique on their own.
func sortByNamespacedName[T any](items []T, key func(T) (namespace, name string)) {
	sort.SliceStable(items, func(i, j int) bool {
		ns1, n1 := key(items[i])
		ns2, n2 := key(items[j])
		if ns1 != ns2 {
			return ns1 < ns2
		}
		return n1 < n2
	})
}
