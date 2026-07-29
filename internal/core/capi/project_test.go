package capi

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// obj builds an unstructured object from a nested map literal, which keeps these
// tests reading like the YAML they stand in for.
func obj(m map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: m}
}

func md(namespace, name string, extra map[string]any) map[string]any {
	md := map[string]any{"namespace": namespace, "name": name}
	for k, v := range extra {
		md[k] = v
	}
	return md
}

// --- Cluster --------------------------------------------------------------

func TestProjectClusters(t *testing.T) {
	snap := ProjectClusters([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "prod", map[string]any{
				"annotations": map[string]any{"cluster.x-k8s.io/paused": ""},
			}),
			"spec": map[string]any{
				"topology": map[string]any{"class": "metal3-class", "version": "v1.32.0"},
			},
			"status": map[string]any{
				"phase": "Provisioned",
				"conditions": []any{
					map[string]any{"type": "Available", "status": "True"},
				},
				"controlPlane": map[string]any{
					"desiredReplicas":  int64(3),
					"replicas":         int64(3),
					"readyReplicas":    int64(3),
					"upToDateReplicas": int64(1),
				},
				"workers": map[string]any{
					"desiredReplicas":  int64(5),
					"upToDateReplicas": int64(5),
				},
			},
		}),
	})

	if len(snap.Items) != 1 {
		t.Fatalf("items: got %d want 1", len(snap.Items))
	}
	c := snap.Items[0]
	if c.Namespace != "capi" || c.Name != "prod" {
		t.Errorf("identity: got %s/%s", c.Namespace, c.Name)
	}
	if c.ClusterClass != "metal3-class" || c.Version != "v1.32.0" {
		t.Errorf("topology: got class=%q version=%q", c.ClusterClass, c.Version)
	}
	if c.Phase != "Provisioned" {
		t.Errorf("phase: got %q", c.Phase)
	}
	if !c.Available {
		t.Error("Available condition True should set Available")
	}
	if !c.Paused {
		t.Error("an empty paused annotation should still count as paused")
	}
	// The control plane is mid-rollout; the workers are done.
	if !c.ControlPlane.Rolling() {
		t.Error("control plane with upToDate < desired should report Rolling")
	}
	if c.Workers.Rolling() {
		t.Error("workers fully up to date should not report Rolling")
	}
	if snap.Err != nil {
		t.Errorf("Err: got %v want nil", snap.Err)
	}
	if snap.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be stamped")
	}
}

func TestProjectClusters_MissingFieldsAreZero(t *testing.T) {
	// A bare object must project without panicking — a partially populated
	// status is normal while a cluster is being created.
	snap := ProjectClusters([]*unstructured.Unstructured{
		obj(map[string]any{"metadata": md("ns", "bare", nil)}),
	})
	c := snap.Items[0]
	if c.Phase != "" || c.Available || c.Paused || c.Version != "" {
		t.Errorf("expected zero values, got %+v", c)
	}
	if c.ControlPlane.Rolling() {
		t.Error("a zero bucket should not report Rolling")
	}
}

// --- rollout replica counts ----------------------------------------------

// v1beta2 renamed updatedReplicas to upToDateReplicas. Both must read, since
// the rollout signal depends on this field.
func TestUpToDateReplicas_BothFieldNames(t *testing.T) {
	newAPI := ProjectKCPs([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "kcp", nil),
			"spec":     map[string]any{"replicas": int64(3)},
			"status":   map[string]any{"upToDateReplicas": int64(2)},
		}),
	})
	if got := newAPI.Items[0].UpToDateReplicas; got != 2 {
		t.Errorf("upToDateReplicas: got %d want 2", got)
	}

	oldAPI := ProjectKCPs([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "kcp", nil),
			"spec":     map[string]any{"replicas": int64(3)},
			"status":   map[string]any{"updatedReplicas": int64(1)},
		}),
	})
	if got := oldAPI.Items[0].UpToDateReplicas; got != 1 {
		t.Errorf("legacy updatedReplicas: got %d want 1", got)
	}
}

// The new name wins when both are present, which happens during a Cluster API
// upgrade while the old field is still being written for compatibility.
func TestUpToDateReplicas_NewFieldWins(t *testing.T) {
	snap := ProjectKCPs([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "kcp", nil),
			"status": map[string]any{
				"upToDateReplicas": int64(3),
				"updatedReplicas":  int64(1),
			},
		}),
	})
	if got := snap.Items[0].UpToDateReplicas; got != 3 {
		t.Errorf("got %d want 3 (upToDateReplicas preferred)", got)
	}
}

// A present-but-zero new field must not fall through to the old one.
func TestUpToDateReplicas_ExplicitZeroIsHonoured(t *testing.T) {
	snap := ProjectKCPs([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "kcp", nil),
			"status": map[string]any{
				"upToDateReplicas": int64(0),
				"updatedReplicas":  int64(5),
			},
		}),
	})
	if got := snap.Items[0].UpToDateReplicas; got != 0 {
		t.Errorf("got %d want 0 — an explicit zero must not fall back", got)
	}
}

func TestProjectKCPs(t *testing.T) {
	snap := ProjectKCPs([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "prod-cp", map[string]any{
				"labels": map[string]any{"cluster.x-k8s.io/cluster-name": "prod"},
			}),
			"spec": map[string]any{"replicas": int64(3), "version": "v1.32.0"},
			"status": map[string]any{
				"replicas":            int64(3),
				"readyReplicas":       int64(2),
				"availableReplicas":   int64(2),
				"unavailableReplicas": int64(1),
				"upToDateReplicas":    int64(2),
				"initialized":         true,
				"conditions":          []any{map[string]any{"type": "Available", "status": "True"}},
			},
		}),
	})
	k := snap.Items[0]
	if k.ClusterName != "prod" {
		t.Errorf("ClusterName: got %q want prod", k.ClusterName)
	}
	if k.Version != "v1.32.0" || k.DesiredReplicas != 3 {
		t.Errorf("spec: got version=%q replicas=%d", k.Version, k.DesiredReplicas)
	}
	if !k.Initialized || !k.Available {
		t.Errorf("status flags: initialized=%v available=%v", k.Initialized, k.Available)
	}
	if !k.Rolling() {
		t.Error("2 of 3 up to date should report Rolling")
	}
}

func TestProjectMachineDeployments(t *testing.T) {
	snap := ProjectMachineDeployments([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "prod-workers", nil),
			"spec": map[string]any{
				"replicas": int64(4),
				"template": map[string]any{"spec": map[string]any{"version": "v1.32.0"}},
			},
			"status": map[string]any{"phase": "Running", "upToDateReplicas": int64(4)},
		}),
	})
	m := snap.Items[0]
	if m.Version != "v1.32.0" {
		t.Errorf("version comes from the template spec: got %q", m.Version)
	}
	if m.Phase != "Running" || m.DesiredReplicas != 4 {
		t.Errorf("got phase=%q desired=%d", m.Phase, m.DesiredReplicas)
	}
	if m.Rolling() {
		t.Error("fully up to date should not report Rolling")
	}
}

func TestProjectMachines(t *testing.T) {
	snap := ProjectMachines([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "prod-cp-abc", map[string]any{
				"labels": map[string]any{"cluster.x-k8s.io/cluster-name": "prod"},
				"ownerReferences": []any{
					map[string]any{"kind": "Node", "name": "unrelated"},
					map[string]any{"kind": "KubeadmControlPlane", "name": "prod-cp"},
				},
			}),
			"spec": map[string]any{
				"version":    "v1.32.0",
				"providerID": "metal3://abc",
				"infrastructureRef": map[string]any{
					"kind": "Metal3Machine",
					"name": "prod-cp-abc-m3m",
				},
			},
			"status": map[string]any{
				"phase":   "Running",
				"nodeRef": map[string]any{"name": "node-1"},
			},
		}),
	})
	m := snap.Items[0]
	if m.OwnerKind != "KubeadmControlPlane" || m.OwnerName != "prod-cp" {
		t.Errorf("owner: got %s/%s want KubeadmControlPlane/prod-cp", m.OwnerKind, m.OwnerName)
	}
	if m.InfraKind != "Metal3Machine" || m.InfraName != "prod-cp-abc-m3m" {
		t.Errorf("infra ref: got %s/%s", m.InfraKind, m.InfraName)
	}
	if m.NodeName != "node-1" {
		t.Errorf("NodeName: got %q want node-1", m.NodeName)
	}
}

func TestProjectMachines_NoRecognisedOwner(t *testing.T) {
	snap := ProjectMachines([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "orphan", map[string]any{
				"ownerReferences": []any{map[string]any{"kind": "Something", "name": "else"}},
			}),
		}),
	})
	if got := snap.Items[0].OwnerKind; got != "" {
		t.Errorf("OwnerKind: got %q want empty", got)
	}
}

// --- Metal3 ---------------------------------------------------------------

func TestProjectMetal3Machines_BMHRef(t *testing.T) {
	snap := ProjectMetal3Machines([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "qualified", map[string]any{
				"annotations": map[string]any{"metal3.io/BareMetalHost": "bmh-ns/host-1"},
			}),
		}),
		obj(map[string]any{
			"metadata": md("capi", "unqualified", map[string]any{
				"annotations": map[string]any{"metal3.io/BareMetalHost": "host-2"},
			}),
		}),
		obj(map[string]any{"metadata": md("capi", "unbound", nil)}),
	})

	byName := map[string]int{}
	for i, m := range snap.Items {
		byName[m.Name] = i
	}

	q := snap.Items[byName["qualified"]]
	if q.BMHNamespace != "bmh-ns" || q.BMHName != "host-1" {
		t.Errorf("qualified ref: got %s/%s want bmh-ns/host-1", q.BMHNamespace, q.BMHName)
	}
	// An unqualified value means the Metal3Machine's own namespace.
	u := snap.Items[byName["unqualified"]]
	if u.BMHNamespace != "capi" || u.BMHName != "host-2" {
		t.Errorf("unqualified ref: got %s/%s want capi/host-2", u.BMHNamespace, u.BMHName)
	}
	// No annotation yet is a normal early state.
	nb := snap.Items[byName["unbound"]]
	if nb.BMHNamespace != "" || nb.BMHName != "" {
		t.Errorf("unbound: got %s/%s want empty", nb.BMHNamespace, nb.BMHName)
	}
}

func TestProjectBareMetalHosts(t *testing.T) {
	snap := ProjectBareMetalHosts([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("bmh-ns", "host-1", nil),
			"spec": map[string]any{
				"online": true,
				"consumerRef": map[string]any{
					"kind": "Metal3Machine", "namespace": "capi", "name": "prod-cp-abc-m3m",
				},
			},
			"status": map[string]any{
				"provisioning":      map[string]any{"state": "provisioned"},
				"operationalStatus": "OK",
				"poweredOn":         true,
				"errorMessage":      "line one\nline two",
			},
		}),
	})
	b := snap.Items[0]
	if b.State != "provisioned" || b.OperationalStatus != "OK" {
		t.Errorf("got state=%q op=%q", b.State, b.OperationalStatus)
	}
	if !b.PoweredOn || !b.Online {
		t.Errorf("power: poweredOn=%v online=%v", b.PoweredOn, b.Online)
	}
	if b.ConsumerName != "prod-cp-abc-m3m" || b.ConsumerNamespace != "capi" {
		t.Errorf("consumer: got %s/%s", b.ConsumerNamespace, b.ConsumerName)
	}
	// Multi-line messages would break table layout.
	if strings.Contains(b.ErrorMessage, "\n") {
		t.Errorf("ErrorMessage should be one line: %q", b.ErrorMessage)
	}
	if b.ErrorMessage != "line one line two" {
		t.Errorf("ErrorMessage: got %q", b.ErrorMessage)
	}
}

func TestProjectMetal3Clusters_ErrorMessagePreferredOverReason(t *testing.T) {
	withBoth := ProjectMetal3Clusters([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "c", nil),
			"status": map[string]any{
				"failureMessage": "could not reach BMC",
				"failureReason":  "BMCError",
			},
		}),
	})
	if got := withBoth.Items[0].ErrorMessage; got != "could not reach BMC" {
		t.Errorf("got %q want the human-readable message", got)
	}

	reasonOnly := ProjectMetal3Clusters([]*unstructured.Unstructured{
		obj(map[string]any{
			"metadata": md("capi", "c", nil),
			"status":   map[string]any{"failureReason": "BMCError"},
		}),
	})
	if got := reasonOnly.Items[0].ErrorMessage; got != "BMCError" {
		t.Errorf("got %q want the reason as fallback", got)
	}
}

// --- conditions -----------------------------------------------------------

func TestConditions(t *testing.T) {
	o := obj(map[string]any{
		"metadata": md("ns", "n", nil),
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "False",
					"reason": "WaitingForHost", "message": "no host\navailable",
					"lastTransitionTime": "2026-07-28T10:00:00Z",
				},
				"not-a-map", // must be skipped, not fatal
				map[string]any{"type": "Available", "status": "True"},
			},
		},
	})

	got := conditions(o)
	if len(got) != 2 {
		t.Fatalf("conditions: got %d want 2 (the non-map entry skipped)", len(got))
	}
	if got[0].Reason != "WaitingForHost" {
		t.Errorf("reason: got %q", got[0].Reason)
	}
	if strings.Contains(got[0].Message, "\n") {
		t.Errorf("message should be collapsed to one line: %q", got[0].Message)
	}
	want := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	if !got[0].LastTransition.Equal(want) {
		t.Errorf("lastTransitionTime: got %v want %v", got[0].LastTransition, want)
	}

	if !hasTrueCondition(o, "Available") {
		t.Error("hasTrueCondition(Available) should be true")
	}
	if hasTrueCondition(o, "Ready") {
		t.Error("hasTrueCondition(Ready) should be false when status is False")
	}
	if hasTrueCondition(o, "Absent") {
		t.Error("hasTrueCondition should be false for a missing condition")
	}
}

func TestConditions_BadTimestampIsIgnored(t *testing.T) {
	got := conditions(obj(map[string]any{
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "lastTransitionTime": "not-a-time"},
		}},
	}))
	if len(got) != 1 || !got[0].LastTransition.IsZero() {
		t.Errorf("an unparseable timestamp should leave a zero time, got %+v", got)
	}
}

// --- events ---------------------------------------------------------------

func TestProjectEvents_NewestFirst(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	snap := ProjectEvents([]*corev1.Event{
		{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "capi", Name: "old"},
			Reason:        "Older",
			LastTimestamp: metav1.NewTime(base.Add(-time.Hour)),
		},
		{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "capi", Name: "new"},
			Reason:        "Newer",
			LastTimestamp: metav1.NewTime(base),
		},
	})
	if len(snap.Items) != 2 {
		t.Fatalf("items: got %d want 2", len(snap.Items))
	}
	if snap.Items[0].Reason != "Newer" {
		t.Errorf("first item: got %q want Newer", snap.Items[0].Reason)
	}
}

// Newer events populate EventTime rather than LastTimestamp; falling back keeps
// them from sorting to the bottom with a zero time.
func TestProjectEvent_FallsBackToEventTime(t *testing.T) {
	when := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
	got := ProjectEvent(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "capi", Name: "e"},
		EventTime:      metav1.NewMicroTime(when),
		Type:           "Warning",
		Reason:         "FailedCreate",
		Message:        "spread\nacross  lines",
		InvolvedObject: corev1.ObjectReference{Kind: "Machine", Name: "m-1"},
		Count:          3,
	})
	if !got.LastTimestamp.Equal(when) {
		t.Errorf("LastTimestamp: got %v want %v", got.LastTimestamp, when)
	}
	// FirstTimestamp defaults to LastTimestamp rather than staying zero.
	if !got.FirstTimestamp.Equal(when) {
		t.Errorf("FirstTimestamp: got %v want %v", got.FirstTimestamp, when)
	}
	if got.Message != "spread across lines" {
		t.Errorf("Message: got %q want whitespace collapsed", got.Message)
	}
	if got.ObjectKind != "Machine" || got.ObjectName != "m-1" || got.Count != 3 {
		t.Errorf("got %+v", got)
	}
}

// --- ordering -------------------------------------------------------------

// Stable ordering keeps tables from shuffling between renders. Namespace is part
// of the key because watching all namespaces means names are not unique.
func TestProjections_SortByNamespaceThenName(t *testing.T) {
	snap := ProjectMachines([]*unstructured.Unstructured{
		obj(map[string]any{"metadata": md("ns-b", "a-machine", nil)}),
		obj(map[string]any{"metadata": md("ns-a", "z-machine", nil)}),
		obj(map[string]any{"metadata": md("ns-a", "a-machine", nil)}),
	})
	var got []string
	for _, m := range snap.Items {
		got = append(got, m.Namespace+"/"+m.Name)
	}
	want := "ns-a/a-machine,ns-a/z-machine,ns-b/a-machine"
	if strings.Join(got, ",") != want {
		t.Errorf("order: got %v want %v", got, want)
	}
}

func TestProjections_EmptyInput(t *testing.T) {
	if snap := ProjectClusters(nil); len(snap.Items) != 0 || snap.Err != nil {
		t.Errorf("got %+v want empty and error-free", snap)
	}
	if snap := ProjectBareMetalHosts(nil); len(snap.Items) != 0 {
		t.Errorf("got %+v want empty", snap)
	}
}

func TestTrimToOneLine(t *testing.T) {
	tests := map[string]string{
		"plain":                    "plain",
		"line one\nline two":       "line one line two",
		"carriage\r\nreturn":       "carriage return",
		"  leading and trailing  ": "leading and trailing",
		"collapse    runs":         "collapse runs",
		"":                         "",
	}
	for in, want := range tests {
		if got := trimToOneLine(in); got != want {
			t.Errorf("trimToOneLine(%q): got %q want %q", in, got, want)
		}
	}
}
