package fleet

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// A kubeconfig with the minimum client-go will accept, so a test can assert
// that a secret's contents were actually parsed rather than merely found.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://cluster.invalid:6443
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
users:
- name: u
  user:
    token: t
`

func kubeconfigSecret(namespace, name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Type:       "cluster.x-k8s.io/secret",
		Data:       map[string][]byte{"value": []byte(testKubeconfig)},
	}
}

func discovererWith(core kubernetes.Interface, dyn dynamic.Interface, namespace string) *Discoverer {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{clusterGVR.GroupVersion()})
	mapper.Add(clusterGK.WithVersion("v1beta2"), meta.RESTScopeNamespace)
	return &Discoverer{dyn: dyn, core: core, mapper: mapper, namespace: namespace}
}

var clusterGVR = schema.GroupVersionResource{
	Group: "cluster.x-k8s.io", Version: "v1beta2", Resource: "clusters",
}

func clusterObject(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	u.SetKind("Cluster")
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func fakeDynamic(objs ...runtime.Object) dynamic.Interface {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme, map[schema.GroupVersionResource]string{clusterGVR: "ClusterList"}, objs...,
	)
}

// The suffix composes with a Cluster's actual name, so a site that suffixes its
// Cluster objects — tenant-01-cluster — resolves without binnacle knowing
// anything about that convention.
func TestWorkloadConfig_ComposesWithTheClusterName(t *testing.T) {
	core := fake.NewSimpleClientset(
		kubeconfigSecret("capi", "tenant-01-cluster-kubeconfig", nil),
	)
	d := discovererWith(core, fakeDynamic(), "capi")

	cfg, err := d.workloadConfig(context.Background(), "capi", "tenant-01-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "https://cluster.invalid:6443" {
		t.Errorf("host = %q; the secret was found but not parsed", cfg.Host)
	}
}

// When upstream's name is absent, the cluster's own label narrows the search.
// This is a fallback, not the primary path.
func TestWorkloadConfig_FallsBackToTheClusterNameLabel(t *testing.T) {
	core := fake.NewSimpleClientset(
		kubeconfigSecret("capi", "some-other-name", map[string]string{
			clusterNameLabel: "tenant-01",
		}),
	)
	d := discovererWith(core, fakeDynamic(), "capi")

	if _, err := d.workloadConfig(context.Background(), "capi", "tenant-01"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Two labeled candidates and no conventional name is a refusal, not a coin
// toss. Handing a cluster the wrong admin credential because the names looked
// similar is the worst outcome available here.
func TestWorkloadConfig_AmbiguityIsRefused(t *testing.T) {
	labels := map[string]string{clusterNameLabel: "tenant-01"}
	core := fake.NewSimpleClientset(
		kubeconfigSecret("capi", "one", labels),
		kubeconfigSecret("capi", "two", labels),
	)
	d := discovererWith(core, fakeDynamic(), "capi")

	_, err := d.workloadConfig(context.Background(), "capi", "tenant-01")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"ambiguous", "one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A missing secret names both what was looked for and what was tried, because
// the operator's next move depends on which it was.
func TestWorkloadConfig_MissingIsExplained(t *testing.T) {
	d := discovererWith(fake.NewSimpleClientset(), fakeDynamic(), "capi")

	_, err := d.workloadConfig(context.Background(), "capi", "tenant-01")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"tenant-01-kubeconfig", clusterNameLabel} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A secret of the right name with no "value" key is a broken credential, not a
// missing one, and must not be reported as absent.
func TestWorkloadConfig_SecretWithoutValueKey(t *testing.T) {
	core := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "capi", Name: "tenant-01-kubeconfig"},
		Data:       map[string][]byte{"ca.crt": []byte("nope")},
	})
	d := discovererWith(core, fakeDynamic(), "capi")

	_, err := d.workloadConfig(context.Background(), "capi", "tenant-01")
	if err == nil || !strings.Contains(err.Error(), "value") {
		t.Fatalf("got %v; want an error naming the missing key", err)
	}
}

// The fleet is the set of Cluster objects, not the set of kubeconfig secrets.
//
// A namespace can hold two naming eras at once, because clusters built before a
// convention changed keep the names they were built with — plus a third secret,
// an operator-maintained copy of the older cluster's kubeconfig under the newer
// name, kept in sync so that tooling assuming the convention still works.
//
// Enumerated from secrets, this reports three clusters. The extra one is the
// worst kind: not a broken orphan but the same live cluster a second time, with
// working credentials and identical numbers, indistinguishable from a real one.
func TestList_DuplicateKubeconfigSecretIsNotASecondCluster(t *testing.T) {
	core := fake.NewSimpleClientset(
		// CAPI's own, for the cluster named under the older convention.
		kubeconfigSecret("capi", "tenant-01-kubeconfig", nil),
		// CAPI's own, for a cluster named under the newer convention.
		kubeconfigSecret("capi", "tenant-02-cluster-kubeconfig", nil),
		// The operator's copy of the first, under the newer convention. No
		// Cluster bears this name.
		kubeconfigSecret("capi", "tenant-01-cluster-kubeconfig", nil),
	)
	dyn := fakeDynamic(
		clusterObject("capi", "tenant-01"),
		clusterObject("capi", "tenant-02-cluster"),
	)
	d := discovererWith(core, dyn, "capi")

	found, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d clusters, want 2 — a duplicate credential became a cluster", len(found))
	}
	for _, f := range found {
		if f.Name == "tenant-01-cluster" {
			t.Error("the copied secret was reported as a cluster of its own")
		}
		// Both naming eras resolve, which is the point: the suffix composes
		// onto each Cluster's real name rather than being a rule about the site.
		if f.Err != nil {
			t.Errorf("%s: unexpected error %v", f.Name, f.Err)
		}
		if f.Config == nil {
			t.Errorf("%s: credentials did not resolve", f.Name)
		}
	}
}

// A cluster whose credentials are missing still appears, carrying the reason.
// A fleet page that silently omits a cluster is answering the wrong question.
func TestList_ClusterWithNoSecretStillAppears(t *testing.T) {
	d := discovererWith(
		fake.NewSimpleClientset(),
		fakeDynamic(clusterObject("capi", "tenant-01")),
		"capi",
	)

	found, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d clusters, want 1", len(found))
	}
	if found[0].Config != nil || found[0].Err == nil {
		t.Errorf("got %+v; want a cluster carrying its credential error", found[0])
	}
}
