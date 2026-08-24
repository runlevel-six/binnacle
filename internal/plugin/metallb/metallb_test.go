package metallb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func stripANSI(s string) string {
	var sb strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// --- settings -------------------------------------------------------------

func TestSettingsFrom(t *testing.T) {
	// An empty block leaves everything to discovery.
	if got := SettingsFrom(nil); got != Defaults() {
		t.Errorf("got %+v want %+v", got, Defaults())
	}
	got := SettingsFrom(map[string]any{"namespace": "net", "speaker_name": "lb-speaker"})
	if got.Namespace != "net" || got.SpeakerName != "lb-speaker" {
		t.Errorf("got %+v", got)
	}
	// A wrong-typed or empty value falls back rather than producing an empty
	// namespace, which would silently read the default namespace instead.
	got = SettingsFrom(map[string]any{"namespace": "", "speaker_name": 42})
	if got != Defaults() {
		t.Errorf("got %+v want the defaults", got)
	}
}

// --- advertisement attribution -------------------------------------------

// An advertisement with no pool list applies to every pool, per MetalLB's own
// semantics. Reading an empty list as "none" would report every pool
// unadvertised on a default installation — the most common configuration there
// is.
func TestAttributeAdvertisements_EmptyListMeansAllPools(t *testing.T) {
	pools := []Pool{{Name: "a"}, {Name: "b"}}
	attributeAdvertisements(pools, []advertisement{{mode: "L2"}})

	for _, p := range pools {
		if strings.Join(p.Advertised, ",") != "L2" {
			t.Errorf("pool %s: got %v want [L2]", p.Name, p.Advertised)
		}
	}
}

func TestAttributeAdvertisements_NamedPools(t *testing.T) {
	pools := []Pool{{Name: "a"}, {Name: "b"}}
	attributeAdvertisements(pools, []advertisement{{mode: "L2", pools: []string{"a"}}})

	if strings.Join(pools[0].Advertised, ",") != "L2" {
		t.Errorf("pool a: got %v", pools[0].Advertised)
	}
	if len(pools[1].Advertised) != 0 {
		t.Errorf("pool b should be unadvertised, got %v", pools[1].Advertised)
	}
}

func TestAttributeAdvertisements_BothModes(t *testing.T) {
	pools := []Pool{{Name: "a"}}
	attributeAdvertisements(pools, []advertisement{
		{mode: "BGP", pools: []string{"a"}},
		{mode: "L2", pools: []string{"a"}},
	})
	// Sorted, so the value is stable across runs.
	if got := strings.Join(pools[0].Advertised, "+"); got != "BGP+L2" {
		t.Errorf("got %q want BGP+L2", got)
	}
}

func TestUnadvertisedPools(t *testing.T) {
	state := State{Pools: []Pool{
		{Name: "advertised", Advertised: []string{"L2"}},
		{Name: "orphan"},
	}}
	got := state.UnadvertisedPools()
	if len(got) != 1 || got[0] != "orphan" {
		t.Errorf("got %v want [orphan]", got)
	}
}

// --- service attribution --------------------------------------------------

// Only an explicit pool annotation attributes a Service. Matching an address back
// to a pool would mean parsing MetalLB's heterogeneous address syntax, which would
// be wrong for a range expressed a way we did not anticipate.
func TestAttributeServices(t *testing.T) {
	pools := []Pool{{Name: "primary"}, {Name: "secondary"}}
	attributeServices(pools, []Service{
		{Name: "a", ExternalIP: "10.0.0.1", Pool: "primary"},
		{Name: "b", ExternalIP: "10.0.0.2", Pool: "primary"},
		// Pending: holds no address, so it is not using one.
		{Name: "c", Pool: "primary"},
		// No annotation: cannot be attributed.
		{Name: "d", ExternalIP: "10.0.0.3"},
		// Unknown pool: ignored rather than mis-attributed.
		{Name: "e", ExternalIP: "10.0.0.4", Pool: "ghost"},
	})

	if pools[0].Assigned != 2 {
		t.Errorf("primary: got %d want 2", pools[0].Assigned)
	}
	if pools[1].Assigned != 0 {
		t.Errorf("secondary: got %d want 0", pools[1].Assigned)
	}
}

func TestPendingServices(t *testing.T) {
	state := State{Services: []Service{
		{Name: "ok", ExternalIP: "10.0.0.1"},
		{Name: "waiting"},
		{Name: "also-waiting"},
	}}
	if got := state.PendingServices(); got != 2 {
		t.Errorf("got %d want 2", got)
	}
}

// --- banner ---------------------------------------------------------------

func TestCells(t *testing.T) {
	tests := []struct {
		name       string
		state      State
		wantStatus tui.BannerStatus
		wantDetail string
	}{
		{
			name:       "healthy",
			state:      State{SpeakerReady: 3, SpeakerDesired: 3, Pools: []Pool{{Name: "a", Advertised: []string{"L2"}}}},
			wantStatus: tui.BannerOK,
		},
		{
			// A speaker that is down means nothing is announced at all, which
			// outranks every other symptom.
			name:       "speaker down",
			state:      State{SpeakerReady: 1, SpeakerDesired: 3},
			wantStatus: tui.BannerErr,
			wantDetail: "speaker 1/3",
		},
		{
			// A pending LoadBalancer usually means the pool is exhausted.
			name: "pending service",
			state: State{
				SpeakerReady: 3, SpeakerDesired: 3,
				Pools:    []Pool{{Name: "a", Advertised: []string{"L2"}}},
				Services: []Service{{Name: "waiting"}},
			},
			wantStatus: tui.BannerWarn,
			wantDetail: "1 pending",
		},
		{
			name: "unadvertised pool",
			state: State{
				SpeakerReady: 3, SpeakerDesired: 3,
				Pools: []Pool{{Name: "orphan"}},
			},
			wantStatus: tui.BannerWarn,
			wantDetail: "unadvertised",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := store.New()
			s.Put(KeyState, tc.state)

			cells := (&Plugin{}).Cells(s)
			if len(cells) != 1 {
				t.Fatalf("got %d cells want 1", len(cells))
			}
			if cells[0].Status != tc.wantStatus {
				t.Errorf("status: got %v want %v", cells[0].Status, tc.wantStatus)
			}
			if tc.wantDetail != "" && !strings.Contains(cells[0].Detail, tc.wantDetail) {
				t.Errorf("detail: got %q want it to contain %q", cells[0].Detail, tc.wantDetail)
			}
		})
	}
}

// Before the first poll there is no cell at all, rather than a misleading one.
func TestCells_NoStateYet(t *testing.T) {
	if got := (&Plugin{}).Cells(store.New()); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

// --- pane -----------------------------------------------------------------

func populatedState() State {
	return State{
		Pools: []Pool{
			{Name: "primary", Addresses: []string{"10.0.0.10-10.0.0.50"}, AutoAssign: true,
				Advertised: []string{"L2"}, Assigned: 3},
			{Name: "manual-only", Addresses: []string{"10.1.0.0/24"}, AutoAssign: false,
				Advertised: []string{"BGP"}},
			{Name: "orphan", Addresses: []string{"10.2.0.1"}, AutoAssign: true},
		},
		Services:       []Service{{Name: "ok", ExternalIP: "10.0.0.11"}, {Name: "waiting"}},
		SpeakerReady:   3,
		SpeakerDesired: 3,
		UpdatedAt:      time.Now(),
	}
}

func TestPane_Render(t *testing.T) {
	s := store.New()
	s.Put(KeyState, populatedState())
	body := stripANSI(newPane(s).Render(90, 12, false))

	for _, want := range []string{"primary", "10.0.0.10-10.0.0.50", "L2", "speaker 3/3"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// An unadvertised pool must stand out, since it hands out addresses nothing
	// announces.
	if !strings.Contains(body, "none") {
		t.Errorf("an unadvertised pool should read as 'none':\n%s", body)
	}
	// A manual-only pool explains an otherwise baffling Pending.
	if !strings.Contains(body, "manual") {
		t.Errorf("a manual-only pool should be marked:\n%s", body)
	}
	if !strings.Contains(body, "pending") {
		t.Errorf("pending services should be reported:\n%s", body)
	}
}

func TestPane_RespectsBounds(t *testing.T) {
	s := store.New()
	s.Put(KeyState, populatedState())
	p := newPane(s)

	for _, w := range []int{20, 40, 90, 200} {
		for _, h := range []int{1, 2, 5, 20} {
			body := p.Render(w, h, false)
			if got := lipgloss.Height(body); body != "" && got > h {
				t.Errorf("%dx%d: %d lines exceeds height", w, h, got)
			}
			for i, line := range strings.Split(body, "\n") {
				if got := lipgloss.Width(line); got > w {
					t.Errorf("%dx%d: line %d width %d exceeds width", w, h, i, got)
				}
			}
		}
	}
}

func TestPane_States(t *testing.T) {
	// No data yet.
	if got := stripANSI(newPane(store.New()).Render(60, 8, false)); !strings.Contains(got, "loading") {
		t.Errorf("got %q want a loading state", got)
	}

	// Present but empty: MetalLB installed with no pools configured yet.
	s := store.New()
	s.Put(KeyState, State{UpdatedAt: time.Now()})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "no address pools") {
		t.Errorf("got %q want an empty state", got)
	}

	// A read failure with no data surfaces the error.
	s = store.New()
	s.Put(KeyState, State{Err: errFake{}, UpdatedAt: time.Now()})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "boom") {
		t.Errorf("got %q want the error surfaced", got)
	}
}

// A partial failure keeps whatever was read, since pools are worth showing even
// when the Service list was denied.
func TestPane_PartialFailureStillRendersPools(t *testing.T) {
	s := store.New()
	state := populatedState()
	state.Err = errFake{}
	s.Put(KeyState, state)

	body := stripANSI(newPane(s).Render(90, 12, false))
	if !strings.Contains(body, "primary") {
		t.Errorf("pools should survive a partial failure:\n%s", body)
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_Identity(t *testing.T) {
	p := newPane(store.New())
	if p.ID() != "metallb" {
		t.Errorf("ID: got %q", p.ID())
	}
	// Important rather than merely useful: load-balancer health belongs with
	// storage and the cloud API, not a tier below them. It is also what places the
	// Network frame first on the bottom row, since among equal priorities the
	// order is registration order.
	if p.Priority() != tui.P1Important {
		t.Errorf("Priority: got %v", p.Priority())
	}
}

// --- speaker discovery ----------------------------------------------------

// The speaker's name depends on how MetalLB was installed. A hardcoded "speaker"
// was wrong on the first real cluster it met — Helm prefixes the release name —
// so it is discovered by suffix instead.
func TestFindSpeaker_DiscoversBySuffix(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
		wantErr string
	}{
		{"upstream manifest", []string{"speaker"}, "speaker", ""},
		{"helm with release prefix", []string{"metallb-speaker"}, "metallb-speaker", ""},
		{"custom release name", []string{"prod-lb-speaker"}, "prod-lb-speaker", ""},
		{
			name:    "alongside other daemonsets",
			present: []string{"node-exporter", "metallb-speaker", "kube-proxy"},
			want:    "metallb-speaker",
		},
		{
			// The error must name what was found, so a site with an
			// unrecognizable name knows what to pin.
			name:    "nothing matches",
			present: []string{"node-exporter", "kube-proxy"},
			wantErr: "node-exporter",
		},
		{"empty namespace", nil, "", "no daemonsets"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.present))
			for _, n := range tc.present {
				objs = append(objs, &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "metallb-system", Name: n},
				})
			}
			p := &Plugin{
				client:   &kube.Client{Typed: fake.NewSimpleClientset(objs...)},
				settings: Defaults(),
			}

			got, err := p.findSpeaker(context.Background(), "metallb-system")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error should mention %q: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("findSpeaker: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// A pinned name is honored without discovery, for a site that renamed it beyond
// recognition.
func TestSpeaker_PinnedNameSkipsDiscovery(t *testing.T) {
	p := &Plugin{
		client: &kube.Client{Typed: fake.NewSimpleClientset(&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "metallb-system", Name: "custom-announcer"},
			Status:     appsv1.DaemonSetStatus{NumberReady: 2, DesiredNumberScheduled: 3},
		})},
		settings: Settings{SpeakerName: "custom-announcer"},
	}

	r, err := p.speaker(context.Background(), "metallb-system")
	if err != nil {
		t.Fatalf("speaker: %v", err)
	}
	if r.Ready != 2 || r.Desired != 3 {
		t.Errorf("got %d/%d want 2/3", r.Ready, r.Desired)
	}
}

func TestSpeaker_DiscoveredNameIsRead(t *testing.T) {
	p := &Plugin{
		client: &kube.Client{Typed: fake.NewSimpleClientset(&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "metallb-system", Name: "metallb-speaker"},
			Status:     appsv1.DaemonSetStatus{NumberReady: 6, DesiredNumberScheduled: 6},
		})},
		settings: Defaults(),
	}

	r, err := p.speaker(context.Background(), "metallb-system")
	if err != nil {
		t.Fatalf("speaker: %v", err)
	}
	if r.Ready != 6 || r.Desired != 6 {
		t.Errorf("got %d/%d want 6/6", r.Ready, r.Desired)
	}
}

// The default leaves the name unset so discovery runs; a default of "speaker"
// would silently fail on any Helm install.
func TestDefaults_LeaveSpeakerNameUnset(t *testing.T) {
	if Defaults().SpeakerName != "" {
		t.Errorf("SpeakerName should be empty so it is discovered, got %q", Defaults().SpeakerName)
	}
}

// --- namespace discovery --------------------------------------------------

// "metallb-system" is only a convention, and it was wrong on the first real
// cluster this met. IPAddressPool objects are namespaced and live alongside the
// installation, which makes them a reliable locator.
func TestNamespaceFor_DerivedFromPools(t *testing.T) {
	p := &Plugin{settings: Defaults()}

	got, err := p.namespaceFor([]Pool{
		{Namespace: "networking", Name: "primary"},
		{Namespace: "networking", Name: "secondary"},
	})
	if err != nil {
		t.Fatalf("namespaceFor: %v", err)
	}
	if got != "networking" {
		t.Errorf("got %q want networking", got)
	}
}

// A pinned namespace wins, for a site whose layout defeats discovery.
func TestNamespaceFor_PinnedWins(t *testing.T) {
	p := &Plugin{settings: Settings{Namespace: "pinned"}}
	got, err := p.namespaceFor([]Pool{{Namespace: "derived"}})
	if err != nil {
		t.Fatalf("namespaceFor: %v", err)
	}
	if got != "pinned" {
		t.Errorf("got %q want pinned", got)
	}
}

// No pools is not an error: MetalLB may be installed with nothing configured yet,
// and there is simply nothing to locate.
func TestNamespaceFor_NoPoolsIsNotAnError(t *testing.T) {
	p := &Plugin{settings: Defaults()}
	got, err := p.namespaceFor(nil)
	if err != nil {
		t.Errorf("no pools should not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// Pools spread across namespaces is not a shape MetalLB supports, so it is
// reported rather than silently resolved by picking one.
func TestNamespaceFor_AmbiguousIsReported(t *testing.T) {
	p := &Plugin{settings: Defaults()}
	_, err := p.namespaceFor([]Pool{{Namespace: "a"}, {Namespace: "b"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"a", "b", "namespace setting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// Discovery must be deterministic despite map iteration.
func TestNamespaceFor_AmbiguousErrorIsStable(t *testing.T) {
	p := &Plugin{settings: Defaults()}
	pools := []Pool{{Namespace: "z"}, {Namespace: "a"}, {Namespace: "m"}}
	_, first := p.namespaceFor(pools)
	for range 20 {
		if _, err := p.namespaceFor(pools); err.Error() != first.Error() {
			t.Fatalf("error is not deterministic:\n%v\n%v", first, err)
		}
	}
}

func TestDefaults_LeaveNamespaceUnset(t *testing.T) {
	if Defaults().Namespace != "" {
		t.Errorf("Namespace should be empty so it is derived, got %q", Defaults().Namespace)
	}
}

// MetalLB is commonly installed into kube-system, where it sits among a dozen
// unrelated DaemonSets — so matching is tight and ordered rather than a loose
// suffix scan.
func TestFindSpeaker_InACrowdedNamespace(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
		wantErr string
	}{
		{
			name: "exact name wins among many",
			present: []string{
				"kube-proxy", "node-exporter", "cilium", "speaker", "csi-rbdplugin",
			},
			want: "speaker",
		},
		{
			name:    "single prefixed name among many",
			present: []string{"kube-proxy", "cilium", "metallb-speaker", "fluent-bit"},
			want:    "metallb-speaker",
		},
		{
			// Picking one would report another workload's readiness as MetalLB's.
			name:    "several candidates is reported",
			present: []string{"metallb-speaker", "other-speaker"},
			wantErr: "several daemonsets",
		},
		{
			// A name merely containing "speaker" without the separator is not a
			// match, which keeps an unrelated workload from being adopted.
			name:    "no separator is not a match",
			present: []string{"kube-proxy", "loudspeaker"},
			wantErr: "no daemonset",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.present))
			for _, n := range tc.present {
				objs = append(objs, &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: n},
				})
			}
			p := &Plugin{client: &kube.Client{Typed: fake.NewSimpleClientset(objs...)}, settings: Defaults()}

			got, err := p.findSpeaker(context.Background(), "kube-system")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error should mention %q: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("findSpeaker: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// End to end: MetalLB in kube-system, located from its pools.
func TestPoll_LocatesMetalLBInKubeSystem(t *testing.T) {
	p := &Plugin{settings: Defaults()}
	ns, err := p.namespaceFor([]Pool{{Namespace: "kube-system", Name: "primary"}})
	if err != nil {
		t.Fatalf("namespaceFor: %v", err)
	}
	if ns != "kube-system" {
		t.Errorf("got %q want kube-system", ns)
	}
}

// --- pool usage -----------------------------------------------------------

// The counts MetalLB publishes on the pool are the answer; reading them is what
// replaced a Service tally that could not see shared or dual-stack addresses.
//
// The numbers are a real v0.15 pool: 10.4.192.12-10.4.192.99 is 88 addresses,
// nine of them out.
func TestPoolUsage_FromStatus(t *testing.T) {
	o := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"assignedIPv4":  int64(9),
			"availableIPv4": int64(79),
			"assignedIPv6":  int64(0),
			"availableIPv6": int64(0),
		},
	}}

	assigned, available, source := poolUsage(o)
	if source != UsageStatus {
		t.Fatalf("source = %v, want UsageStatus", source)
	}
	if assigned != 9 || available != 79 {
		t.Errorf("got %d assigned / %d available, want 9 / 79", assigned, available)
	}
	if total := (Pool{Assigned: assigned, Available: available}).Total(); total != 88 {
		t.Errorf("total = %d, want 88", total)
	}
}

// A dual-stack pool is one budget as far as "nearly full" goes.
func TestPoolUsage_SumsBothFamilies(t *testing.T) {
	o := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"assignedIPv4": int64(2), "availableIPv4": int64(8),
			"assignedIPv6": int64(3), "availableIPv6": int64(7),
		},
	}}
	if assigned, available, _ := poolUsage(o); assigned != 5 || available != 15 {
		t.Errorf("got %d / %d, want 5 / 15", assigned, available)
	}
}

// A MetalLB too old to publish the status must not be read as an empty pool:
// "nothing is using this" and "cannot tell" are different things to show someone
// deciding whether a pool is safe to delete.
func TestPoolUsage_AbsentStatusIsUnknown(t *testing.T) {
	o := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"addresses": []any{"10.0.0.1-10.0.0.9"}},
	}}
	if _, _, source := poolUsage(o); source != UsageUnknown {
		t.Errorf("source = %v, want UsageUnknown", source)
	}
}

// An empty pool does publish a status, and it is not the same as no status.
func TestPoolUsage_ZeroAssignedIsStillKnown(t *testing.T) {
	o := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"assignedIPv4": int64(0), "availableIPv4": int64(10)},
	}}
	assigned, available, source := poolUsage(o)
	if source != UsageStatus || assigned != 0 || available != 10 {
		t.Errorf("got %d / %d (%v), want 0 / 10 (UsageStatus)", assigned, available, source)
	}
}

// An unstructured object that has been through a plain JSON round trip carries
// float64, and a count silently read as zero would look like an idle pool.
func TestPoolUsage_ToleratesFloatCounts(t *testing.T) {
	o := unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"assignedIPv4": float64(9), "availableIPv4": float64(79)},
	}}
	if assigned, available, source := poolUsage(o); source != UsageStatus || assigned != 9 || available != 79 {
		t.Errorf("got %d / %d (%v), want 9 / 79 (UsageStatus)", assigned, available, source)
	}
}

// --- service annotations --------------------------------------------------

// The bug: sextant read only the request annotation, which nobody sets on a
// cluster using autoAssign, so every pool reported nothing in use.
func TestPoolOf_PrefersWhatMetalLBWrote(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{
			// The whole population of a real v0.15 cluster looked like this: an
			// allocation annotation and a pinned address, and no request for a
			// pool anywhere.
			name: "allocation annotation only",
			annotations: map[string]string{
				"metallb.io/ip-allocated-from-pool":          "default",
				"metallb.universe.tf/ip-allocated-from-pool": "default",
				"metallb.universe.tf/loadBalancerIPs":        "10.4.192.99",
			},
			want: "default",
		},
		{
			// One Service on that cluster had only the newer prefix, having been
			// reallocated after the upgrade. Preferring the legacy key loses it.
			name:        "only the metallb.io prefix",
			annotations: map[string]string{"metallb.io/ip-allocated-from-pool": "default"},
			want:        "default",
		},
		{
			name:        "legacy prefix alone still works",
			annotations: map[string]string{"metallb.universe.tf/ip-allocated-from-pool": "old"},
			want:        "old",
		},
		{
			// A pinned Service before MetalLB has allocated: the request is all
			// there is, and it is still worth attributing.
			name:        "request annotation",
			annotations: map[string]string{"metallb.universe.tf/address-pool": "manual"},
			want:        "manual",
		},
		{
			// Allocation wins over request: where they disagree, what happened
			// beats what was asked for.
			name: "allocation beats request",
			annotations: map[string]string{
				"metallb.io/ip-allocated-from-pool": "actual",
				"metallb.io/address-pool":           "wanted",
			},
			want: "actual",
		},
		{name: "nothing", annotations: map[string]string{}, want: ""},
		{
			name:        "unrelated metallb annotations do not count",
			annotations: map[string]string{"metallb.universe.tf/allow-shared-ip": "yes"},
			want:        "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolOf(tc.annotations); got != tc.want {
				t.Errorf("poolOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// The published counts are better than anything a Service tally can produce, so
// the fallback must not overwrite them.
func TestAttributeServices_LeavesPublishedCountsAlone(t *testing.T) {
	pools := []Pool{
		{Name: "measured", Assigned: 9, Available: 79, Usage: UsageStatus},
		{Name: "unmeasured"},
	}
	attributeServices(pools, []Service{
		{Name: "a", ExternalIP: "10.0.0.1", Pool: "measured"},
		{Name: "b", ExternalIP: "10.0.0.2", Pool: "unmeasured"},
	})

	if pools[0].Assigned != 9 || pools[0].Usage != UsageStatus {
		t.Errorf("published count was overwritten: %d (%v)", pools[0].Assigned, pools[0].Usage)
	}
	if pools[1].Assigned != 1 || pools[1].Usage != UsageAnnotations {
		t.Errorf("fallback did not count: %d (%v)", pools[1].Assigned, pools[1].Usage)
	}
}

// A pool nothing names keeps UsageUnknown, so the pane can say so rather than
// showing a zero that reads as an idle pool.
func TestAttributeServices_UnnamedPoolStaysUnknown(t *testing.T) {
	pools := []Pool{{Name: "lonely"}}
	attributeServices(pools, []Service{{Name: "a", ExternalIP: "10.0.0.1"}})
	if pools[0].Usage != UsageUnknown {
		t.Errorf("usage = %v, want UsageUnknown", pools[0].Usage)
	}
}

func TestExhaustedPools(t *testing.T) {
	state := State{Pools: []Pool{
		{Name: "full", Assigned: 10, Available: 0, Usage: UsageStatus},
		{Name: "roomy", Assigned: 1, Available: 9, Usage: UsageStatus},
		// Never allocated from, so not "exhausted" — an empty pool with no
		// capacity is a misconfiguration, not a pool that filled up.
		{Name: "empty", Usage: UsageStatus},
		// No published counts, so nothing can be claimed about it.
		{Name: "unknown", Usage: UsageUnknown},
	}}
	got := state.ExhaustedPools()
	if len(got) != 1 || got[0] != "full" {
		t.Errorf("got %v, want [full]", got)
	}
}

// --- the IN USE cell ------------------------------------------------------

func TestUsageCell(t *testing.T) {
	tests := []struct {
		name string
		pool Pool
		want string
	}{
		// The number an operator wants is not how many are out but how many are
		// left, so the cell carries both.
		{"published", Pool{Assigned: 9, Available: 79, Usage: UsageStatus}, "9/88"},
		{"empty pool", Pool{Assigned: 0, Available: 88, Usage: UsageStatus}, "0/88"},
		{"full pool", Pool{Assigned: 88, Available: 0, Usage: UsageStatus}, "88/88"},
		// No honest total exists, so the bare count stands alone rather than
		// inventing a denominator.
		{"counted from annotations", Pool{Assigned: 3, Usage: UsageAnnotations}, "3"},
		// Not zero: a zero reads as an idle pool, and an idle pool gets deleted.
		{"unmeasurable", Pool{Usage: UsageUnknown}, "?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := usage(tc.pool); got != tc.want {
				t.Errorf("usage = %q, want %q", got, tc.want)
			}
		})
	}
}

// Running out of addresses is the failure this pane exists to see coming, so a
// pool near its limit must not look like any other row.
func TestUsageCell_ColorsAPoolRunningOut(t *testing.T) {
	plain := func(p Pool) string { _, st := usage(p); return st.Render("x") }

	roomy := plain(Pool{Assigned: 10, Available: 90, Usage: UsageStatus})
	nearly := plain(Pool{Assigned: 95, Available: 5, Usage: UsageStatus})
	full := plain(Pool{Assigned: 100, Available: 0, Usage: UsageStatus})

	if roomy == nearly {
		t.Error("a pool with a tenth left looks like one that is barely used")
	}
	if nearly == full {
		t.Error("an exhausted pool looks like one that is merely close")
	}
}

// End to end on the shape that produced the bug report: a v0.15 cluster where
// every LoadBalancer carries an allocation annotation and none carries a
// request. The pane used to render 0 for each pool.
func TestPane_ShowsUsageOnAnAutoAssignCluster(t *testing.T) {
	pools := []Pool{
		{Namespace: "kube-system", Name: "default",
			Addresses: []string{"10.4.192.12-10.4.192.99"}, AutoAssign: true,
			Advertised: []string{"L2"}, Assigned: 9, Available: 79, Usage: UsageStatus},
		{Namespace: "kube-system", Name: "rook-rgw-replication",
			Addresses: []string{"10.252.4.20-10.252.4.20"}, AutoAssign: true,
			Advertised: []string{"L2"}, Assigned: 0, Available: 1, Usage: UsageStatus},
	}
	services := make([]Service, 0, 9)
	for i := range 9 {
		services = append(services, Service{
			Namespace: "mgd-rabbitmq", Name: fmt.Sprintf("svc-%d", i),
			ExternalIP: fmt.Sprintf("10.4.192.%d", 15+i),
			// Only what MetalLB wrote; no request annotation anywhere.
			Pool: "default",
		})
	}

	s := store.New()
	s.Put(KeyState, State{
		Pools: pools, Services: services, Namespace: "kube-system",
		SpeakerReady: 3, SpeakerDesired: 3, UpdatedAt: time.Now(),
	})
	body := stripANSI(newPane(s).Render(100, 12, false))

	if !strings.Contains(body, "9/88") {
		t.Errorf("the default pool does not report its usage:\n%s", body)
	}
	if !strings.Contains(body, "0/1") {
		t.Errorf("the unused pool does not report its capacity:\n%s", body)
	}
	if strings.Contains(body, "Service(s) pending") {
		t.Errorf("nothing is pending, but the summary says otherwise:\n%s", body)
	}
}

// A full pool is a LoadBalancer that will not come up next time anyone asks, and
// the point of MetalLB publishing counts is to say so before that happens.
func TestCells_WarnsBeforeAPoolRunsOut(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{
		Pools: []Pool{{Name: "default", Advertised: []string{"L2"},
			Assigned: 88, Available: 0, Usage: UsageStatus}},
		Services:       []Service{{Name: "ok", ExternalIP: "10.0.0.1"}},
		SpeakerReady:   3,
		SpeakerDesired: 3,
	})

	cells := (&Plugin{}).Cells(s)
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1", len(cells))
	}
	if cells[0].Status != tui.BannerWarn {
		t.Errorf("status = %v, want a warning", cells[0].Status)
	}
	if !strings.Contains(cells[0].Detail, "default") {
		t.Errorf("detail %q does not name the full pool", cells[0].Detail)
	}
}
