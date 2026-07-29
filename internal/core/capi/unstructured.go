package capi

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/runlevel-six/sextant/internal/core/model"
)

// Field readers for *unstructured.Unstructured. They return the zero value on
// any error, which is right for a read-only observer: a field that is missing
// now will be re-read on the next informer event.

func uStr(obj *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj.Object, fields...)
	return v
}

func uInt32(obj *unstructured.Unstructured, fields ...string) int32 {
	v, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return int32(v)
}

func uBool(obj *unstructured.Unstructured, fields ...string) bool {
	v, _, _ := unstructured.NestedBool(obj.Object, fields...)
	return v
}

// uAge returns the time since metadata.creationTimestamp.
func uAge(obj *unstructured.Unstructured) time.Duration {
	t := obj.GetCreationTimestamp().Time
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// Well-known Cluster API and Metal3 annotations and labels.
const (
	annotationPaused        = "cluster.x-k8s.io/paused"
	annotationBareMetalHost = "metal3.io/BareMetalHost"
	labelClusterName        = "cluster.x-k8s.io/cluster-name"
)

// pausedAnnotation reports whether the paused annotation is present.
//
// Presence alone is truthy, whatever the value, matching how Cluster API treats
// it: `kubectl annotate ... paused=""` pauses just as `paused="true"` does.
func pausedAnnotation(obj *unstructured.Unstructured) bool {
	_, ok := obj.GetAnnotations()[annotationPaused]
	return ok
}

// clusterNameLabel returns the Cluster API label naming the owning Cluster.
func clusterNameLabel(obj *unstructured.Unstructured) string {
	return obj.GetLabels()[labelClusterName]
}

// hasTrueCondition reports whether a status condition of the given type is True.
func hasTrueCondition(obj *unstructured.Unstructured, condType string) bool {
	for _, c := range conditions(obj) {
		if c.Type == condType {
			return c.Status == "True"
		}
	}
	return false
}

// conditions reads .status.conditions into the shared condition type. Cluster
// API, Metal3 and core Kubernetes shapes are compatible enough to share this.
func conditions(obj *unstructured.Unstructured) []model.Condition {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	out := make([]model.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := model.Condition{}
		if v, ok := m["type"].(string); ok {
			c.Type = v
		}
		if v, ok := m["status"].(string); ok {
			c.Status = v
		}
		if v, ok := m["reason"].(string); ok {
			c.Reason = v
		}
		if v, ok := m["message"].(string); ok {
			c.Message = trimToOneLine(v)
		}
		if v, ok := m["lastTransitionTime"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				c.LastTransition = t
			}
		}
		out = append(out, c)
	}
	return out
}

// firstOwnerOfKind returns the first ownerReference matching any of kinds.
//
// Used to attribute a Machine to its KubeadmControlPlane or MachineSet, the
// latter of which rolls up to a MachineDeployment.
func firstOwnerOfKind(obj *unstructured.Unstructured, kinds ...string) (kind, name string) {
	want := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	for _, ref := range obj.GetOwnerReferences() {
		if _, ok := want[ref.Kind]; ok {
			return ref.Kind, ref.Name
		}
	}
	return "", ""
}

// upToDateReplicas reads the up-to-date replica count from a rollout resource.
//
// Cluster API v1beta2 renamed status.updatedReplicas to status.upToDateReplicas.
// The new name is read first and the old one is the fallback, so the same code
// works against both. This is the field the rollout signal depends on, so
// getting the fallback right matters more than it looks.
func upToDateReplicas(obj *unstructured.Unstructured) int32 {
	if v, ok, _ := unstructured.NestedInt64(obj.Object, "status", "upToDateReplicas"); ok {
		return int32(v)
	}
	v, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
	return int32(v)
}

// trimToOneLine collapses a multi-line message so it cannot break table layout.
// Applied at the projection boundary, not at render time.
func trimToOneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// API groups. Versions are deliberately absent: kinds are resolved to a
// GroupVersionResource through the discovery RESTMapper at runtime, so Cluster
// API's v1beta1 to v1beta2 to v1 progression needs no change here. Pinning a
// version is the usual reason third-party Cluster API tooling breaks on upgrade.
const (
	groupCAPI        = "cluster.x-k8s.io"
	groupCAPIControl = "controlplane.cluster.x-k8s.io"
	groupCAPIInfra   = "infrastructure.cluster.x-k8s.io"
	groupMetal3      = "metal3.io"
)

// Kinds watched on the management cluster.
var (
	gkCluster             = schema.GroupKind{Group: groupCAPI, Kind: "Cluster"}
	gkMachine             = schema.GroupKind{Group: groupCAPI, Kind: "Machine"}
	gkMachineDeployment   = schema.GroupKind{Group: groupCAPI, Kind: "MachineDeployment"}
	gkKubeadmControlPlane = schema.GroupKind{Group: groupCAPIControl, Kind: "KubeadmControlPlane"}
	gkMetal3Cluster       = schema.GroupKind{Group: groupCAPIInfra, Kind: "Metal3Cluster"}
	gkMetal3Machine       = schema.GroupKind{Group: groupCAPIInfra, Kind: "Metal3Machine"}
	gkBareMetalHost       = schema.GroupKind{Group: groupMetal3, Kind: "BareMetalHost"}
)
