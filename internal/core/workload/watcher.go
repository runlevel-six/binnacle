// Package workload watches a workload cluster's core resources — nodes, pods,
// events and the three workload kinds — and publishes them as model snapshots.
//
// Everything site-specific is supplied by a profile: which label keys carry a
// node's role, and which namespaces' events are worth surfacing. Nothing in this
// package names a namespace, a label, or a workload.
package workload

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/runlevel-six/sextant/internal/core"
	"github.com/runlevel-six/sextant/internal/core/capi"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

// resyncPeriod is how often informers do a full relist, so a missed watch event
// self-heals within one refresh a user would notice.
const resyncPeriod = 30 * time.Second

// Options configures a Watcher.
type Options struct {
	// NodeRoles supplies the label keys that carry a node's role.
	NodeRoles profile.NodeRoles
	// Events supplies the namespace filter for the events stream.
	Events profile.Events
}

// Watcher observes one workload cluster.
type Watcher struct {
	store *store.Store
	opts  Options
	typed kubernetes.Interface
}

// New builds a Watcher. It performs no I/O; call Run to start watching.
func New(cfg *rest.Config, s *store.Store, opts Options) (*Watcher, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	return &Watcher{store: s, opts: opts, typed: typed}, nil
}

// maxScopedEventNamespaces bounds how many namespaces get their own event
// informer before the watch widens to the whole cluster.
//
// Two costs pull against each other and one of them dominates. N scoped informers
// mean N watch connections and N list calls; one cluster-wide informer means one
// of each. But a cluster-wide event *list* pays for every event in the cluster,
// and events are the highest-volume object there is — a cluster mid-maintenance,
// or one running a workflow engine, has tens of thousands of them, almost all in
// namespaces the filter then discards. Measured against a real cluster, that list
// took minutes and left the pane blank for all of them, while a handful of extra
// watch connections costs nothing anyone can perceive. So the bound is set where
// connection count starts to look silly rather than where it starts to cost.
const maxScopedEventNamespaces = 24

// resolveEventNamespaces decides how to scope the event watch, returning the
// namespaces to watch and, when it has to widen to the whole cluster, why.
//
// A prefix is expanded by listing namespaces once at startup, which is the whole
// point: it turns "every event in the cluster" into "the events in these six
// namespaces". An earlier version treated a prefix as unresolvable and fell back
// to a cluster-wide watch, on the reasoning that namespaces come and go while the
// dashboard runs. That trade was the wrong way round — a namespace created
// mid-session is a rare miss recoverable by restarting, while listing every event
// on a busy cluster is a guaranteed blank pane for minutes.
//
// An empty result means watch nothing, which is right when a profile names no
// namespaces and no prefixes: the filter would discard every event anyway, so
// listing them all to throw them all away is pure waste.
func (w *Watcher) resolveEventNamespaces(ctx context.Context) ([]string, string) {
	f := w.opts.Events
	if f.AllNamespaces {
		return nil, "the profile asks for every namespace"
	}

	set := make(map[string]bool, len(f.Namespaces))
	for _, ns := range f.Namespaces {
		if ns != "" {
			set[ns] = true
		}
	}

	if len(f.NamespacePrefixes) > 0 {
		list, err := w.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			// Without the namespace list the prefixes cannot be resolved, so the
			// only way to honor them is to watch everything. Say so: this is the
			// slow path, and a reader deserves to know it was not chosen.
			return nil, "cannot list namespaces to expand the profile's prefixes (" + err.Error() + ")"
		}
		for _, ns := range list.Items {
			for _, prefix := range f.NamespacePrefixes {
				if prefix != "" && strings.HasPrefix(ns.Name, prefix) {
					set[ns.Name] = true
				}
			}
		}
	}

	if len(set) > maxScopedEventNamespaces {
		return nil, fmt.Sprintf("%d namespaces match, more than the %d that are watched individually",
			len(set), maxScopedEventNamespaces)
	}

	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, ""
}

// Run starts the informers and blocks until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.typed, resyncPeriod)

	nodes := factory.Core().V1().Nodes()
	pods := factory.Core().V1().Pods()
	deploys := factory.Apps().V1().Deployments()
	stses := factory.Apps().V1().StatefulSets()
	dses := factory.Apps().V1().DaemonSets()

	// Events get their own factories so they can be scoped independently of the
	// cluster-scoped resources above — one per watched namespace, or a single
	// cluster-wide one when the filter cannot be reduced to a namespace list.
	eventNS, wideReason := w.resolveEventNamespaces(ctx)
	eventFactories := []informers.SharedInformerFactory{}
	var eventListers []corelisters.EventLister
	var eventInformers []cache.SharedIndexInformer
	for _, ns := range eventNS {
		f := informers.NewSharedInformerFactoryWithOptions(
			w.typed, resyncPeriod, informers.WithNamespace(ns))
		eventFactories = append(eventFactories, f)
		eventListers = append(eventListers, f.Core().V1().Events().Lister())
		eventInformers = append(eventInformers, f.Core().V1().Events().Informer())
	}
	// A cluster-wide watch only when scoping was impossible; an empty namespace
	// list with no reason means the profile asked for nothing and nothing is
	// watched.
	if len(eventFactories) == 0 && wideReason != "" {
		eventListers = append(eventListers, factory.Core().V1().Events().Lister())
		eventInformers = append(eventInformers, factory.Core().V1().Events().Informer())
	}

	// Nodes and pods are republished together because node resource columns are
	// computed by joining the two. Publishing them independently would let the
	// panes show requests that do not correspond to the node list beside them.
	publishNodesAndPods := func() {
		nodeList, nErr := nodes.Lister().List(labels.Everything())
		podList, pErr := pods.Lister().List(labels.Everything())

		if pErr != nil {
			w.store.Put(model.KeyWorkloadPods, model.ErrorSnapshot(pErr))
		} else {
			w.store.Put(model.KeyWorkloadPods, ProjectPods(podList))
		}

		switch {
		case nErr != nil:
			w.store.Put(model.KeyWorkloadNodes, model.ErrorSnapshot(nErr))
		case pErr != nil:
			// Nodes are still worth showing without resource attribution.
			w.store.Put(model.KeyWorkloadNodes, ProjectNodes(nodeList, nil, w.opts.NodeRoles))
		default:
			w.store.Put(model.KeyWorkloadNodes, ProjectNodes(nodeList, podList, w.opts.NodeRoles))
		}
	}

	publishWorkloads := func() {
		var out []model.Workload
		var firstErr error

		if list, err := deploys.Lister().List(labels.Everything()); err != nil {
			firstErr = err
		} else {
			out = append(out, ProjectDeployments(list)...)
		}
		if list, err := stses.Lister().List(labels.Everything()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			out = append(out, ProjectStatefulSets(list)...)
		}
		if list, err := dses.Lister().List(labels.Everything()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			out = append(out, ProjectDaemonSets(list)...)
		}

		if firstErr != nil && len(out) == 0 {
			w.store.Put(model.KeyWorkloadWorkloads, model.ErrorSnapshot(firstErr))
			return
		}
		SortWorkloads(out)
		w.store.Put(model.KeyWorkloadWorkloads, model.Snapshot[model.Workload]{
			Items: out, UpdatedAt: time.Now(), Err: firstErr,
		})
	}

	// eventsPublished gates the watch-error reporting below: an error is only news
	// until the events stream has produced a real result.
	var eventsPublished atomic.Bool
	publishEvents := func() {
		var raw []*corev1.Event
		for _, lister := range eventListers {
			items, err := lister.List(labels.Everything())
			if err != nil {
				w.store.Put(model.KeyWorkloadEvents, model.ErrorSnapshot(err))
				return
			}
			raw = append(raw, items...)
		}
		eventsPublished.Store(true)
		// The filter still runs even over scoped listers, since a scoped watch
		// narrows what is fetched but the filter is what decides what is shown —
		// and the two can disagree when a prefix forced a cluster-wide watch.
		w.store.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{
			Items:     FilterEvents(raw, w.opts.Events, capi.ProjectEvent),
			UpdatedAt: time.Now(),
		})
	}

	// Rebuilds are coalesced, because a snapshot rebuild re-lists and re-sorts
	// every object of its kind. Pods and events are the high-churn resources on a
	// real cluster — a single Event that has occurred many thousands of times is
	// one object being updated continuously — and rebuilding per callback made the
	// dashboard slow to appear and then stuttery.
	triggerNodesAndPods := core.Coalesce(ctx, core.PublishInterval, publishNodesAndPods)
	triggerWorkloads := core.Coalesce(ctx, core.PublishInterval, publishWorkloads)
	triggerEvents := core.Coalesce(ctx, core.PublishInterval, publishEvents)

	onChange(nodes.Informer(), triggerNodesAndPods)
	onChange(pods.Informer(), triggerNodesAndPods)
	for _, i := range eventInformers {
		onChange(i, triggerEvents)
	}
	onChange(deploys.Informer(), triggerWorkloads)
	onChange(stses.Informer(), triggerWorkloads)
	onChange(dses.Informer(), triggerWorkloads)

	// A failing event watch reports itself rather than looking like an empty
	// cluster. Registered before Start, since that is the only time it is allowed.
	w.reportEventWatchFailures(eventInformers, eventsPublished.Load)

	factory.Start(ctx.Done())
	for _, f := range eventFactories {
		f.Start(ctx.Done())
	}

	// Publish once per snapshot as soon as *its* caches are warm.
	//
	// Informer handlers only fire on add, update and delete, so a resource with no
	// objects never triggers one. A cluster with no events in the watched
	// namespaces would leave that key unwritten, which a pane cannot tell apart
	// from a source that failed to start — it shows "loading" indefinitely.
	// "Nothing to report" is a result and has to be published as one.
	//
	// Each group waits only for the informers it reads, in its own goroutine,
	// because one slow watch must not hold the others back. The cluster-wide event
	// list is by far the slowest thing here — it is what a profile with namespace
	// prefixes forces, and on a busy cluster it can outlast a diagnostic's whole
	// timeout. Waiting on the shared factory made every other key wait behind it,
	// which defeated the purpose of publishing at all.
	waitThenPublish(ctx, publishNodesAndPods, nodes.Informer(), pods.Informer())
	waitThenPublish(ctx, publishWorkloads, deploys.Informer(), stses.Informer(), dses.Informer())
	// Say what the events pane is waiting for before it starts waiting. A
	// cluster-wide list can run for minutes on a busy cluster, and "loading" for
	// minutes with no reason given is indistinguishable from broken.
	if note := eventSyncNote(wideReason); note != "" {
		w.store.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{Note: note})
	}
	waitThenPublish(ctx, publishEvents, eventInformers...)

	<-ctx.Done()
	return ctx.Err()
}

// waitThenPublish publishes one snapshot once the informers behind it have synced.
//
// Returns immediately; the wait happens on its own goroutine. A canceled context
// publishes nothing, since a partial cache would be reported as a complete result.
func waitThenPublish(ctx context.Context, publish func(), informers ...cache.SharedIndexInformer) {
	synced := make([]cache.InformerSynced, 0, len(informers))
	for _, i := range informers {
		synced = append(synced, i.HasSynced)
	}
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), synced...) {
			publish()
		}
	}()
}

// reportEventWatchFailures surfaces a failing event watch in the pane.
//
// Without this, a credential that cannot list events cluster-wide produces an
// informer that retries forever, publishes nothing, and leaves the events pane
// saying "loading" for the life of the session — with the actual "forbidden"
// message going only to klog, which is not on screen. The permission is easy to
// miss because a *namespace-scoped* event watch may well be allowed while the
// cluster-wide one a namespace prefix forces is not.
//
// Only reported until the first successful publish. After that the watch is known
// to work, and a transient error — an expired resource version, a brief
// disconnect — is something the informer recovers from on its own and must not
// replace good data with a red pane.
func (w *Watcher) reportEventWatchFailures(informers []cache.SharedIndexInformer, published func() bool) {
	for _, i := range informers {
		// A handler registration that fails is not worth failing the watcher over:
		// the pane simply keeps saying "loading", which is the old behavior.
		_ = i.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			if published() {
				return
			}
			w.store.Put(model.KeyWorkloadEvents, model.ErrorSnapshot(err))
		})
	}
}

// onChange republishes on every add, update and delete.
//
// A full rebuild per event is deliberate. Incremental snapshot maintenance would
// be faster but has to be exactly right about deletions and out-of-order
// updates; relisting from the informer cache is in-memory, and cannot drift from
// what the cache holds.
func onChange(informer cache.SharedIndexInformer, rebuild func()) {
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { rebuild() },
		UpdateFunc: func(_, _ any) { rebuild() },
		DeleteFunc: func(any) { rebuild() },
	})
}

// eventSyncNote describes what the event watch is doing while its cache fills, or
// returns the empty string when there is nothing worth saying.
//
// Only the slow path gets a note. A scoped watch over a few namespaces syncs fast
// enough that a message would flash and vanish, which is worse than silence.
func eventSyncNote(wideReason string) string {
	if wideReason == "" {
		return ""
	}
	return fmt.Sprintf("listing events in every namespace — %s. This can take "+
		"minutes on a busy cluster; narrow events.namespaces in your profile to avoid it.", wideReason)
}
