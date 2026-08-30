// Package metallb reports MetalLB's state: which address pools exist, how they
// are advertised, whether the speaker is running, and which LoadBalancer Services
// are still waiting for an address.
//
// Those four together answer the questions a bare-metal operator actually has —
// "is the speaker up" and "are we about to run out of addresses" — neither of
// which is visible from any single object.
//
// This is the simplest plugin shape: everything comes from the API, so there is no
// exec and no informer-only tier to fall back to. It is either present or absent.
package metallb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	metallbstate "github.com/runlevel-six/sextant/pkg/subsystem/metallb"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// Name is the plugin's registration name.
const Name = "metallb"

// Datastore keys.
const (
	// KeyState holds a State.
	KeyState = metallbstate.KeyState
)

// API group and kinds. Versions are absent so the RESTMapper resolves them, which
// is what keeps this working across MetalLB releases.
const group = "metallb.io"

var (
	gkIPAddressPool    = schema.GroupKind{Group: group, Kind: "IPAddressPool"}
	gkL2Advertisement  = schema.GroupKind{Group: group, Kind: "L2Advertisement"}
	gkBGPAdvertisement = schema.GroupKind{Group: group, Kind: "BGPAdvertisement"}
)

// pollInterval is how often the state is refreshed. MetalLB's objects change
// rarely, so this is deliberately unhurried.
const pollInterval = 15 * time.Second

// The MetalLB state types live in pkg/subsystem/metallb so a consumer outside
// this module can read them. Aliased here so this package's own code is
// unchanged.
// The usage sources, aliased alongside their type.
const (
	UsageUnknown     = metallbstate.UsageUnknown
	UsageStatus      = metallbstate.UsageStatus
	UsageAnnotations = metallbstate.UsageAnnotations
)

type (
	// Pool is an alias for [metallbstate.Pool].
	Pool = metallbstate.Pool
	// UsageSource is an alias for [metallbstate.UsageSource].
	UsageSource = metallbstate.UsageSource
	// Service is an alias for [metallbstate.Service].
	Service = metallbstate.Service
	// State is an alias for [metallbstate.State].
	State = metallbstate.State
)

// Settings is the plugin's profile configuration.
type Settings struct {
	// Namespace pins where MetalLB is installed. Leave it empty to derive it,
	// which is the default — see namespaceFor.
	Namespace string
	// SpeakerName pins the speaker DaemonSet's name. Leave it empty to discover
	// it, which is the default — see findSpeaker.
	SpeakerName string
}

// Defaults leave both the namespace and the speaker name unset so each is
// derived from the cluster.
//
// Two hardcoded guesses were wrong on the first real cluster they met. The name
// varies because the upstream manifest installs "speaker" while Helm prefixes the
// release name; the namespace varies because "metallb-system" is only a
// convention. Rather than guess again, both are discovered — and there is a
// reliable signal for it, since IPAddressPool objects live in MetalLB's own
// namespace.
func Defaults() Settings { return Settings{} }

// SettingsFrom reads a profile's plugin block over the defaults.
func SettingsFrom(raw map[string]any) Settings {
	s := Defaults()
	if v, ok := raw["namespace"].(string); ok && v != "" {
		s.Namespace = v
	}
	if v, ok := raw["speaker_name"].(string); ok && v != "" {
		s.SpeakerName = v
	}
	return s
}

// Plugin observes MetalLB.
type Plugin struct {
	client   *kube.Client
	dyn      dynamic.Interface
	settings Settings
}

// New builds the plugin.
func New(c *kube.Client, dyn dynamic.Interface, settings Settings) *Plugin {
	return &Plugin{client: c, dyn: dyn, settings: settings}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return Name }

// Detect reports whether MetalLB's CRDs are registered.
//
// The CRD is the right probe rather than the speaker DaemonSet: a cluster can have
// MetalLB installed with the speaker temporarily scaled to zero, and that is a
// state worth showing rather than hiding.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	return p.client.HasKind(group, "IPAddressPool"), nil
}

// Run polls until ctx is canceled.
func (p *Plugin) Run(ctx context.Context, s *store.Store) error {
	publish := func() { s.Put(KeyState, p.poll(ctx)) }
	publish() // once immediately, so the pane is never stuck on "loading"

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			publish()
		}
	}
}

// poll reads everything once and assembles a State.
//
// A failure in one part does not discard the others: a denied read of Services
// still leaves the pools worth showing, so the error is recorded alongside
// whatever was obtained.
func (p *Plugin) poll(ctx context.Context) State {
	state := State{UpdatedAt: time.Now()}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	pools, err := p.listPools(ctx)
	note(err)
	services, err := p.listServices(ctx)
	note(err)
	adverts := p.listAdvertisements(ctx)

	// The pools locate MetalLB, so the speaker is looked for where they live
	// rather than in an assumed namespace.
	namespace, err := p.namespaceFor(pools)
	note(err)
	if namespace != "" {
		rollout, speakerErr := p.speaker(ctx, namespace)
		note(speakerErr)
		state.Rollout = rollout
		state.SpeakerReady = rollout.Ready
		state.SpeakerDesired = rollout.Desired
		state.Namespace = namespace
	}

	attributeAdvertisements(pools, adverts)
	attributeServices(pools, services)

	state.Pools = pools
	state.Services = services
	state.Err = firstErr
	return state
}

// namespaceFor decides which namespace MetalLB occupies.
//
// A configured namespace wins. Otherwise it is taken from the IPAddressPool
// objects, which are namespaced and live alongside the installation — a far more
// reliable signal than the "metallb-system" convention, which was wrong on the
// first cluster this met.
//
// Pools in several namespaces is not a shape MetalLB supports, so it is reported
// rather than silently picking one.
func (p *Plugin) namespaceFor(pools []Pool) (string, error) {
	if p.settings.Namespace != "" {
		return p.settings.Namespace, nil
	}
	seen := map[string]bool{}
	for _, pool := range pools {
		if pool.Namespace != "" {
			seen[pool.Namespace] = true
		}
	}
	switch len(seen) {
	case 0:
		return "", nil // no pools yet; nothing to locate, and not an error
	case 1:
		for ns := range seen {
			return ns, nil
		}
	}
	return "", fmt.Errorf(
		"IPAddressPools span %d namespaces (%s); pin one with the namespace setting",
		len(seen), strings.Join(sortedKeys(seen), ", "))
}

func (p *Plugin) listPools(ctx context.Context) ([]Pool, error) {
	objs, err := p.list(ctx, gkIPAddressPool)
	if err != nil {
		return nil, err
	}
	out := make([]Pool, 0, len(objs))
	for _, o := range objs {
		// autoAssign defaults to true when absent, per MetalLB's schema.
		autoAssign := true
		if v, found, _ := unstructured.NestedBool(o.Object, "spec", "autoAssign"); found {
			autoAssign = v
		}
		addrs, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "addresses")
		pool := Pool{
			Namespace:  o.GetNamespace(),
			Name:       o.GetName(),
			Addresses:  addrs,
			AutoAssign: autoAssign,
		}
		pool.Assigned, pool.Available, pool.Usage = poolUsage(o)
		out = append(out, pool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// poolUsage reads the address counts MetalLB publishes on an IPAddressPool.
//
// MetalLB does this arithmetic itself and puts the answer in the object's
// status, which is worth far more than anything this program could work out.
// Counting Services cannot see two Services sharing one address, or one Service
// holding both an IPv4 and an IPv6; and deriving a pool's size means parsing an
// address list that may be a CIDR, a dashed range or a bare address, which is
// the parsing this package has always declined to do. Reading the status skips
// all of it, and answers "how much is left" — a question no amount of Service
// counting can reach.
//
// The status subresource is not in every MetalLB release, so its absence is
// reported rather than read as an empty pool; see [UsageSource].
func poolUsage(o unstructured.Unstructured) (assigned, available int, source UsageSource) {
	var found bool
	read := func(field string) int {
		n, ok := nestedCount(o.Object, "status", field)
		found = found || ok
		return n
	}
	// Both families are summed. A dual-stack pool's addresses are one budget as
	// far as "is this pool nearly full" is concerned, and splitting them out
	// would spend a column on a distinction most clusters do not have.
	assigned = read("assignedIPv4") + read("assignedIPv6")
	available = read("availableIPv4") + read("availableIPv6")
	if !found {
		return 0, 0, UsageUnknown
	}
	return assigned, available, UsageStatus
}

// nestedCount reads a non-negative integer, tolerating either JSON number shape.
//
// Kubernetes' own decoder yields int64 for whole numbers, but an unstructured
// object that has been through a plain encoding/json round trip carries float64,
// and a count silently read as zero would be indistinguishable from a pool with
// nothing in it.
func nestedCount(obj map[string]any, fields ...string) (int, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !found {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// advertisement is an L2 or BGP advertisement.
type advertisement struct {
	mode  string
	pools []string
}

// listAdvertisements reads both advertisement kinds.
//
// It cannot fail: a cluster using only L2 has no BGPAdvertisement CRD registered
// at all, so an absent kind is the normal case rather than an error.
func (p *Plugin) listAdvertisements(ctx context.Context) []advertisement {
	var out []advertisement

	for _, spec := range []struct {
		gk   schema.GroupKind
		mode string
	}{
		{gkL2Advertisement, "L2"},
		{gkBGPAdvertisement, "BGP"},
	} {
		objs, err := p.list(ctx, spec.gk)
		if err != nil {
			// One advertisement kind may legitimately be absent — a cluster
			// using only L2 has no BGPAdvertisement CRD at all.
			continue
		}
		for _, o := range objs {
			pools, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "ipAddressPools")
			out = append(out, advertisement{mode: spec.mode, pools: pools})
		}
	}
	return out
}

// attributeAdvertisements marks which pools are advertised and how.
//
// An advertisement with no pool list applies to every pool, which is MetalLB's
// own semantics — reading an empty list as "none" would report every pool
// unadvertised on a default installation.
func attributeAdvertisements(pools []Pool, adverts []advertisement) {
	for i := range pools {
		modes := map[string]bool{}
		for _, a := range adverts {
			if len(a.pools) == 0 || containsString(a.pools, pools[i].Name) {
				modes[a.mode] = true
			}
		}
		pools[i].Advertised = sortedKeys(modes)
	}
}

func (p *Plugin) listServices(ctx context.Context) ([]Service, error) {
	list, err := p.client.Typed.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	out := make([]Service, 0)
	for _, svc := range list.Items {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		s := Service{
			Namespace: svc.Namespace,
			Name:      svc.Name,
			Pool:      poolOf(svc.Annotations),
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				s.ExternalIP = ing.IP
				break
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// poolAnnotations are the Service annotations naming a pool, best first.
//
// The first two are written by MetalLB itself when it allocates an address, and
// are what makes this work at all: the last two are *requests*, set only by an
// operator pinning a Service to a particular pool, and on a cluster using
// autoAssign — which is the ordinary case — nobody sets them. Reading only the
// request annotation is why every pool reported nothing in use however many
// LoadBalancers were working.
//
// Order matters, and not only for tidiness. MetalLB is migrating from the
// metallb.universe.tf prefix to metallb.io, and mid-migration the two disagree:
// on a v0.15 cluster, eight of nine Services carried both keys and the ninth —
// reallocated after the upgrade — carried only the metallb.io one. Preferring
// the older prefix would have lost that Service.
var poolAnnotations = []string{
	"metallb.io/ip-allocated-from-pool",
	"metallb.universe.tf/ip-allocated-from-pool",
	"metallb.io/address-pool",
	"metallb.universe.tf/address-pool",
}

// poolOf returns the pool a Service's annotations name, or "".
func poolOf(annotations map[string]string) string {
	for _, key := range poolAnnotations {
		if v := annotations[key]; v != "" {
			return v
		}
	}
	return ""
}

// attributeServices counts Services per pool, for pools whose own status did not
// already say.
//
// This is the fallback, not the answer: see [poolUsage] for why MetalLB's
// published counts are better wherever they exist. It counts Services rather
// than addresses, so it is an undercount on a cluster where Services share an
// address or hold one of each family — which is why it records itself as
// [UsageAnnotations] rather than passing for the real thing.
//
// A pool no annotation names is left [UsageUnknown] rather than set to zero. On
// a MetalLB too old to publish either, "nothing is using this pool" and "this
// build cannot tell" are very different things to show an operator deciding
// whether a pool is safe to remove.
func attributeServices(pools []Pool, services []Service) {
	index := map[string]int{}
	for i, p := range pools {
		if pools[i].Usage == UsageStatus {
			continue
		}
		index[p.Name] = i
	}
	for _, s := range services {
		if s.Pool == "" || s.Pending() {
			continue
		}
		if i, ok := index[s.Pool]; ok {
			pools[i].Assigned++
			pools[i].Usage = UsageAnnotations
		}
	}
}

// speakerSuffix is what every MetalLB speaker DaemonSet name ends with, whatever
// release prefix precedes it.
const speakerSuffix = "speaker"

// speaker reports the speaker DaemonSet's readiness and rollout state.
//
// The name is discovered rather than assumed, because it depends on how MetalLB
// was installed: the upstream manifest calls it "speaker" and Helm prefixes the
// release name. A pinned name in a profile is honored first, for a site that has
// renamed it beyond recognition.
func (p *Plugin) speaker(ctx context.Context, namespace string) (kube.Rollout, error) {
	name := p.settings.SpeakerName
	var err error
	if name == "" {
		name, err = p.findSpeaker(ctx, namespace)
		if err != nil {
			return kube.Rollout{}, err
		}
	}
	ds, err := p.client.Typed.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return kube.Rollout{}, fmt.Errorf("get daemonset %s/%s: %w", namespace, name, err)
	}
	r := kube.RolloutOfDaemonSet(ds)
	if !r.Converged() {
		r.StaleNodes = p.client.StaleNodes(ctx, ds)
	}
	return r, nil
}

// findSpeaker locates the speaker DaemonSet by name.
//
// Matching the name rather than a label, because MetalLB's labels have themselves
// changed across releases and installation methods while the name has always
// ended in "speaker".
//
// Matching is deliberately tight, and ordered. An exact "speaker" wins; failing
// that, exactly one name ending in "-speaker". A bare suffix match would be
// risky now that the namespace is discovered rather than assumed: MetalLB is
// commonly installed into kube-system, where it sits among a dozen unrelated
// DaemonSets. Several candidates is reported rather than resolved by guessing,
// since picking the wrong one would report another workload's readiness as
// MetalLB's.
func (p *Plugin) findSpeaker(ctx context.Context, namespace string) (string, error) {
	list, err := p.client.Typed.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list daemonsets in %s: %w", namespace, err)
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("no daemonsets in namespace %s", namespace)
	}

	var suffixed []string
	names := make([]string, 0, len(list.Items))
	for _, ds := range list.Items {
		names = append(names, ds.Name)
		switch {
		case ds.Name == speakerSuffix:
			return ds.Name, nil
		case strings.HasSuffix(ds.Name, "-"+speakerSuffix):
			suffixed = append(suffixed, ds.Name)
		}
	}

	sort.Strings(suffixed)
	switch len(suffixed) {
	case 1:
		return suffixed[0], nil
	case 0:
		sort.Strings(names)
		return "", fmt.Errorf(
			"no daemonset in %s is named %q or ends in %q (found: %s); pin one with the speaker_name setting",
			namespace, speakerSuffix, "-"+speakerSuffix, strings.Join(names, ", "))
	}
	return "", fmt.Errorf(
		"several daemonsets in %s could be the speaker (%s); pin one with the speaker_name setting",
		namespace, strings.Join(suffixed, ", "))
}

func (p *Plugin) list(ctx context.Context, gk schema.GroupKind) ([]unstructured.Unstructured, error) {
	mapping, err := p.client.Mapper.RESTMapping(gk)
	if err != nil {
		return nil, fmt.Errorf("%s is not available: %w", gk, err)
	}
	list, err := p.dyn.Resource(mapping.Resource).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", gk, err)
	}
	return list.Items, nil
}

// Cells implements plugin.BannerProvider.
func (p *Plugin) Cells(s *store.Store) []tui.BannerCell {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		return nil
	}

	cell := tui.BannerCell{Name: "MetalLB"}
	switch {
	case state.Err != nil:
		cell.Status = tui.BannerWarn
		cell.Detail = "read error"
	case state.SpeakerDesired > 0 && state.SpeakerReady < state.SpeakerDesired:
		cell.Status = tui.BannerErr
		cell.Detail = fmt.Sprintf("speaker %d/%d", state.SpeakerReady, state.SpeakerDesired)
	case state.PendingServices() > 0:
		// A pending LoadBalancer usually means the pool is exhausted, which is
		// the failure this plugin exists to make visible.
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d pending", state.PendingServices())
	case len(state.ExhaustedPools()) > 0:
		// Ahead of the pending Service rather than after it. A full pool is a
		// LoadBalancer that will not come up the next time anyone asks for one,
		// and the whole value of MetalLB publishing its own counts is that this
		// can be said before the failure rather than during it.
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%s full", strings.Join(state.ExhaustedPools(), ", "))
	case len(state.UnadvertisedPools()) > 0:
		cell.Status = tui.BannerWarn
		cell.Detail = fmt.Sprintf("%d pool(s) unadvertised", len(state.UnadvertisedPools()))
	default:
		cell.Status = tui.BannerOK
	}
	return []tui.BannerCell{cell}
}

// Panes implements plugin.PaneProvider.
func (p *Plugin) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{newPane(s)}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
