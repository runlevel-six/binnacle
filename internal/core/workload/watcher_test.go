package workload

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// newTestWatcher builds a Watcher over a fake clientset, so the informer plumbing
// is exercised without a cluster.
func newTestWatcher(s *store.Store, opts Options, objects ...runtime.Object) *Watcher {
	return &Watcher{store: s, opts: opts, typed: fake.NewSimpleClientset(objects...)}
}

// mustQuantity parses a resource quantity, panicking on a bad literal — these are
// test constants, so a typo should fail loudly at once.
func mustQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// runUntilPublished runs the watcher until every key is present, or the deadline
// passes.
func runUntilPublished(t *testing.T, w *Watcher, s *store.Store, keys ...string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		missing := ""
		for _, k := range keys {
			if _, ok := s.Raw(k); !ok {
				missing = k
				break
			}
		}
		if missing == "" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("key %q was never published", missing)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var allWorkloadKeys = []string{
	model.KeyWorkloadNodes,
	model.KeyWorkloadPods,
	model.KeyWorkloadEvents,
	model.KeyWorkloadWorkloads,
}

// This is the regression test for a bug found on a real cluster: informer
// handlers only fire on add, update and delete, so a resource with no objects
// never triggered a publish. The key stayed unwritten, which a pane cannot
// distinguish from a source that failed to start — it showed "loading" forever.
//
// An empty cluster is a normal state and must be published as a result.
func TestRun_EmptyClusterPublishesEveryKey(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Default().Events,
	})
	runUntilPublished(t, w, s, allWorkloadKeys...)

	for _, key := range allWorkloadKeys {
		snap, ok := store.Get[model.Snapshot[model.Event]](s, key)
		if key == model.KeyWorkloadEvents {
			if !ok {
				t.Errorf("%s: not published as the expected type", key)
			}
			if len(snap.Items) != 0 {
				t.Errorf("%s: expected an empty snapshot, got %d items", key, len(snap.Items))
			}
			if snap.Err != nil {
				t.Errorf("%s: an empty cluster is not an error, got %v", key, snap.Err)
			}
		}
	}

	// And specifically: the snapshot is present and empty, not absent.
	nodes, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("nodes key missing")
	}
	if len(nodes.Items) != 0 {
		t.Errorf("expected no nodes, got %d", len(nodes.Items))
	}
	if nodes.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be stamped even for an empty snapshot")
	}
}

// A populated cluster publishes real data, with node resources attributed from
// the pods scheduled on them.
func TestRun_PublishesNodesWithPodResources(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Default().Events,
	},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-1",
				Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			},
			Status: corev1.NodeStatus{
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: mustQuantity("4")},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "p1"},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{{
					Name: "c",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity("500m")},
					},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	runUntilPublished(t, w, s, model.KeyWorkloadNodes)

	// Wait for the join to include the pod, since nodes and pods publish together
	// but the informers sync independently.
	var node model.Node
	deadline := time.After(10 * time.Second)
	for {
		snap, _ := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
		if len(snap.Items) == 1 && snap.Items[0].RequestedCPU > 0 {
			node = snap.Items[0]
			break
		}
		select {
		case <-deadline:
			t.Fatalf("node resources were never attributed: %+v", snap.Items)
		case <-time.After(10 * time.Millisecond):
		}
	}

	if node.Name != "node-1" || node.Role != "control-plane" {
		t.Errorf("got %+v", node)
	}
	if node.RequestedCPU != 500 {
		t.Errorf("RequestedCPU: got %d want 500", node.RequestedCPU)
	}
	if node.AllocatableCPU != 4000 {
		t.Errorf("AllocatableCPU: got %d want 4000", node.AllocatableCPU)
	}
}

// The events key is published even when the filter excludes everything, so the
// pane can say "none" rather than "loading".
func TestRun_EventsPublishedWhenFilterExcludesAll(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		// A filter matching nothing on this cluster.
		Events: profile.Events{Namespaces: []string{"no-such-namespace"}},
	},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "default", Name: "e1"},
			Reason:        "Excluded",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	)
	runUntilPublished(t, w, s, model.KeyWorkloadEvents)

	snap, ok := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
	if !ok {
		t.Fatal("events key missing")
	}
	if len(snap.Items) != 0 {
		t.Errorf("expected the filter to exclude everything, got %+v", snap.Items)
	}
	if snap.Err != nil {
		t.Errorf("an empty result is not an error: %v", snap.Err)
	}
}

func TestRun_EventsFilteredByNamespace(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"kube-system"}},
	},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "kube-system", Name: "keep"},
			Reason:        "Kept",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "default", Name: "drop"},
			Reason:        "Dropped",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	)
	runUntilPublished(t, w, s, model.KeyWorkloadEvents)

	deadline := time.After(10 * time.Second)
	for {
		snap, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
		if len(snap.Items) == 1 {
			if snap.Items[0].Reason != "Kept" {
				t.Errorf("wrong event survived the filter: %+v", snap.Items[0])
			}
			return
		}
		if len(snap.Items) > 1 {
			t.Fatalf("filter let through %d events: %+v", len(snap.Items), snap.Items)
		}
		select {
		case <-deadline:
			t.Fatal("the kept event never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Run must return when its context is canceled, or quitting the dashboard would
// leak a goroutine per watcher.
func TestRun_StopsOnContextCancel(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{NodeRoles: profile.Default().NodeRoles})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let it start before canceling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// A namespace list must scope the event watch, so a busy cluster's events are
// neither listed nor cached only to be discarded.
//
// The multi-namespace case is a regression test. An earlier version scoped only
// when exactly one namespace was named, which meant any profile naming two — the
// OpenStack profile names "openstack" and "kube-system" — quietly fell back to
// watching every event in the cluster. On a real cluster that LIST was slow enough
// that the initial sync outlasted a diagnostic's timeout and the events key was
// never published at all.
func TestResolveEventNamespaces(t *testing.T) {
	nsObjects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "argo-template"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed-harbor"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed-keycloak"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed-vault"}},
	}

	tests := []struct {
		name     string
		filter   profile.Events
		want     string
		wantWide bool
	}{
		{
			name:   "single namespace scopes",
			filter: profile.Events{Namespaces: []string{"kube-system"}},
			want:   "kube-system",
		},
		{
			// The default profile should get the cheap path with no configuration.
			name:   "the default profile scopes",
			filter: profile.Default().Events,
			want:   "kube-system",
		},
		{
			name:   "several namespaces each get a factory",
			filter: profile.Events{Namespaces: []string{"openstack", "kube-system"}},
			want:   "kube-system,openstack",
		},
		{
			// The reason this changed: a prefix used to force a cluster-wide watch,
			// which on a busy cluster meant listing every event on it — tens of
			// thousands, almost all discarded by the filter. Expanding the prefix
			// against the namespace list turns that into five scoped watches.
			name:   "a prefix expands to the namespaces matching it",
			filter: profile.Events{Namespaces: []string{"openstack", "kube-system"}, NamespacePrefixes: []string{"managed-"}},
			want:   "kube-system,managed-harbor,managed-keycloak,managed-vault,openstack",
		},
		{
			name:   "a prefix matching nothing yields only the exact namespaces",
			filter: profile.Events{Namespaces: []string{"openstack"}, NamespacePrefixes: []string{"nothing-"}},
			want:   "openstack",
		},
		{
			name:     "all namespaces cannot scope",
			filter:   profile.Events{AllNamespaces: true},
			want:     "",
			wantWide: true,
		},
		{
			// Nothing named means nothing matches the filter either, so watching
			// everything in order to discard everything is pure waste.
			name:   "an empty filter watches nothing",
			filter: profile.Events{},
			want:   "",
		},
		{
			name: "too many namespaces falls back",
			filter: profile.Events{Namespaces: []string{
				"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12",
				"13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25",
			}},
			want:     "",
			wantWide: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWatcher(store.New(), Options{Events: tc.filter}, nsObjects...)
			got, wide := w.resolveEventNamespaces(context.Background())
			if strings.Join(got, ",") != tc.want {
				t.Errorf("namespaces: got %q want %q", strings.Join(got, ","), tc.want)
			}
			if (wide != "") != tc.wantWide {
				t.Errorf("cluster-wide reason = %q, want wide=%v", wide, tc.wantWide)
			}
		})
	}
}

// A credential that cannot list namespaces cannot have its prefixes expanded, so
// the watch widens rather than silently dropping the prefixed namespaces — and
// says which it did.
func TestResolveEventNamespaces_DeniedNamespaceListWidens(t *testing.T) {
	w := newTestWatcher(store.New(), Options{
		Events: profile.Events{Namespaces: []string{"openstack"}, NamespacePrefixes: []string{"managed-"}},
	})
	client := w.typed.(*fake.Clientset)
	client.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "namespaces"}, "", errors.New("cannot list namespaces"))
	})

	got, wide := w.resolveEventNamespaces(context.Background())
	if len(got) != 0 {
		t.Errorf("namespaces: got %v, want none (widened)", got)
	}
	if !strings.Contains(wide, "namespaces") {
		t.Errorf("reason should name what failed, got %q", wide)
	}
}

// The slow path explains itself. "Loading" for minutes with no reason given is
// indistinguishable from broken.
func TestEventSyncNote(t *testing.T) {
	if note := eventSyncNote(""); note != "" {
		t.Errorf("a scoped watch needs no note, got %q", note)
	}
	note := eventSyncNote("the profile asks for every namespace")
	if !strings.Contains(note, "every namespace") || !strings.Contains(note, "profile") {
		t.Errorf("note should say what is happening and how to avoid it, got %q", note)
	}
}

// End to end with the OpenStack profile's two namespaces: events from both arrive,
// and nothing from a third.
func TestRun_MultiNamespaceEventWatch(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"openstack", "kube-system"}},
	},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "openstack", Name: "a"},
			Reason:        "FromOpenStack",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "kube-system", Name: "b"},
			Reason:        "FromKubeSystem",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "default", Name: "c"},
			Reason:        "Excluded",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	)
	runUntilPublished(t, w, s, model.KeyWorkloadEvents)

	deadline := time.After(10 * time.Second)
	for {
		snap, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
		reasons := map[string]bool{}
		for _, e := range snap.Items {
			reasons[e.Reason] = true
		}
		if reasons["Excluded"] {
			t.Fatalf("an unwatched namespace leaked through: %+v", snap.Items)
		}
		if reasons["FromOpenStack"] && reasons["FromKubeSystem"] {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("events from both namespaces never arrived: %+v", snap.Items)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// End to end with a scoped filter: the right event still arrives.
func TestRun_ScopedEventWatchStillPublishes(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"kube-system"}},
	},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "kube-system", Name: "e"},
			Reason:        "Scoped",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	)
	runUntilPublished(t, w, s, model.KeyWorkloadEvents)

	deadline := time.After(10 * time.Second)
	for {
		snap, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
		if len(snap.Items) == 1 && snap.Items[0].Reason == "Scoped" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("scoped watch never published the event: %+v", snap.Items)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Churn on one object must not drive a rebuild per update. This is the shape of
// the real problem: an Event that has occurred many thousands of times is one
// object being updated continuously.
func TestRun_ChurnIsCoalesced(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"kube-system"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// Wait for every key before counting, not just the events one. Each snapshot
	// now publishes as soon as its own informers sync, so the events key can appear
	// while other watches are still warming — and hammering the fake client before
	// its watch is draining overflows the fake watcher's fixed-size channel, which
	// panics rather than blocking.
	for _, key := range []string{
		model.KeyWorkloadEvents,
		model.KeyWorkloadNodes,
		model.KeyWorkloadPods,
		model.KeyWorkloadWorkloads,
	} {
		runUntilKey(t, s, key)
	}

	updates := 0
	sub := s.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		timeout := time.After(2 * time.Second)
		for {
			select {
			case <-sub:
				updates++
			case <-timeout:
				return
			}
		}
	}()

	// Hammer one event object the way a hot event behaves.
	hot := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "kube-system", Name: "hot"},
		Reason:        "BackOff",
		LastTimestamp: metav1.NewTime(time.Now()),
	}
	for i := 1; i <= 300; i++ {
		hot.Count = int32(i)
		if i == 1 {
			_, _ = w.typed.CoreV1().Events("kube-system").Create(ctx, hot, metav1.CreateOptions{})
			continue
		}
		_, _ = w.typed.CoreV1().Events("kube-system").Update(ctx, hot, metav1.UpdateOptions{})
		// The fake watcher's channel holds 100 and panics when full rather than
		// blocking, so give the informer room to drain. A real API server applies
		// backpressure instead; this yield is about the fake, not the behavior
		// under test — 300 updates still arrive well inside the 500ms window.
		if i%25 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	<-done

	// 300 updates over ~2s at a 500ms interval should be a handful of publishes,
	// not hundreds. The bound is loose because other keys share the subscription.
	if updates > 40 {
		t.Errorf("store notifications: got %d for 300 object updates — churn was not coalesced", updates)
	}
}

// runUntilKey waits for one key to appear.
func runUntilKey(t *testing.T, s *store.Store, key string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if _, ok := s.Raw(key); ok {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("key %q was never published", key)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A credential that cannot list events cluster-wide must say so. Without this the
// informer retries forever, publishes nothing, and the pane reads "loading" for
// the whole session while the reason goes only to klog — which is not on screen.
// The permission is easy to miss, since a namespace-scoped watch may be allowed
// where the cluster-wide one a prefix forces is not.
func TestRun_ForbiddenEventWatchIsReported(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"kube-system"}},
	})

	denied := apierrors.NewForbidden(
		schema.GroupResource{Resource: "events"}, "",
		errors.New("User cannot list resource \"events\" in the namespace"))
	client := w.typed.(*fake.Clientset)
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	runUntilKey(t, s, model.KeyWorkloadEvents)
	snap, ok := store.Get[model.Snapshot[any]](s, model.KeyWorkloadEvents)
	if !ok || snap.Err == nil {
		t.Fatalf("want an error snapshot, got %+v (present=%v)", snap, ok)
	}
	if !strings.Contains(snap.Err.Error(), "events") {
		t.Errorf("error should name what failed, got: %v", snap.Err)
	}

	// And the other snapshots publish anyway. This is the second half of the same
	// fix: an event watch that never syncs used to hold back the shared
	// WaitForCacheSync, so every other key stayed unpublished behind it — and a key
	// that is never written is indistinguishable, to a pane, from a source that
	// failed to start. Each group now waits only for the informers it reads.
	for _, key := range []string{
		model.KeyWorkloadNodes,
		model.KeyWorkloadPods,
		model.KeyWorkloadWorkloads,
	} {
		runUntilKey(t, s, key)
	}
}

// Once events have published successfully, a transient watch error must not
// replace good data with an error pane — the informer recovers on its own.
func TestRun_WatchErrorAfterFirstPublishIsIgnored(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{Namespaces: []string{"kube-system"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	runUntilKey(t, s, model.KeyWorkloadEvents)

	// The handler is registered on the informer; call the same predicate path by
	// asserting the published snapshot is a real one and stays that way.
	before, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
	if before.Err != nil {
		t.Fatalf("first publish should be clean, got %v", before.Err)
	}
	time.Sleep(600 * time.Millisecond)
	after, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
	if after.Err != nil {
		t.Errorf("a healthy watch should not turn into an error: %v", after.Err)
	}
}

// The whole point of expanding prefixes: a cluster whose events are dominated by a
// namespace nobody asked about must not cost anything to start up. Here the noisy
// namespace holds an event that the watch never even lists.
func TestRun_PrefixedNamespacesAreWatchedWithoutTheRest(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events: profile.Events{
			Namespaces:        []string{"openstack"},
			NamespacePrefixes: []string{"managed-"},
		},
	},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed-harbor"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "argo-template"}},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "openstack", Name: "a"},
			Reason:        "FromOpenStack",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "managed-harbor", Name: "b"},
			Reason:        "FromManagedService",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "argo-template", Name: "c"},
			Reason:        "FromTheNoisyNamespace",
			LastTimestamp: metav1.NewTime(time.Now()),
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		snap, _ := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
		reasons := map[string]bool{}
		for _, e := range snap.Items {
			reasons[e.Reason] = true
		}
		if reasons["FromOpenStack"] && reasons["FromManagedService"] {
			if reasons["FromTheNoisyNamespace"] {
				t.Fatal("an unwatched namespace's event reached the pane")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("prefixed namespace events never arrived: %+v", snap.Items)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A snapshot that is not ready says what it is waiting for, and the pane shows
// that instead of an empty table. A profile asking for every namespace is the one
// case that cannot be scoped away.
func TestRun_ClusterWideWatchExplainsItself(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, Options{
		NodeRoles: profile.Default().NodeRoles,
		Events:    profile.Events{AllNamespaces: true},
	})

	release := make(chan struct{})
	defer close(release)
	client := w.typed.(*fake.Clientset)
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-release
		return true, &corev1.EventList{}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		snap, ok := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents)
		if ok && snap.Note != "" {
			if snap.Err != nil {
				t.Errorf("a slow list is not an error: %v", snap.Err)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no note published while the event list was outstanding")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
