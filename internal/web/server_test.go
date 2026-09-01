package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/subsystem"
	"github.com/runlevel-six/sextant/pkg/subsystem/cilium"
	"github.com/runlevel-six/sextant/pkg/subsystem/metallb"
	"github.com/runlevel-six/sextant/pkg/subsystem/openstack"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
)

type fakeFleet struct {
	clusters []fleet.ClusterView
	detail   map[string]fleet.ClusterDetail
	changed  chan struct{}
}

func (f *fakeFleet) View() []fleet.ClusterView { return f.clusters }
func (f *fakeFleet) Changed() <-chan struct{}  { return f.changed }

func (f *fakeFleet) Cluster(namespace, name string) (fleet.ClusterDetail, bool) {
	d, ok := f.detail[namespace+"/"+name]
	return d, ok
}

func serve(t *testing.T, clusters ...fleet.ClusterView) http.Handler {
	t.Helper()
	s, err := New(&fakeFleet{clusters: clusters, changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestFleetPage_RendersClusters(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", Version: "v1.31.4", Phase: "Provisioned",
		Status:       health.StatusOK,
		Cells:        []health.Cell{{Name: "Nodes", Status: health.StatusOK}},
		ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3},
		Workers:      model.ReplicaBucket{Desired: 5, Ready: 5},
		Pools: []fleet.NodePool{
			{Role: "Control Plane", Name: "tenant-01-cp", Ready: 3, Desired: 3, Version: "v1.31.4"},
			{Role: "Workers", Name: "tenant-01-md-0", Ready: 5, Desired: 5, Version: "v1.31.4"},
		},
		Nodes:      fleet.NodeCount{Ready: 8, Total: 8},
		NodesKnown: true,
		Capacity: fleet.Capacity{
			CPURequested: 1000, CPUAllocatable: 4000,
			MemRequested: 1 << 30, MemAllocatable: 4 << 30,
		},
		Workloads:     []fleet.WorkloadCount{{Kind: "Deployment", Ready: 9, Total: 10}},
		UnhealthyPods: 2,
		UpdatedAt:     time.Now(),
	})
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"tenant-01", "capi", "v1.31.4", "3/3", "5/5", "8/8", "Nodes",
		"tenant-01-cp", "Control Plane", "25%", "Deployment 9/10", "2 unhealthy pods",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q", want)
		}
	}
}

// The severity has to reach the markup as something a stylesheet can key on.
// It is the only channel by which sextant's verdict becomes visible.
func TestFleetPage_StatusReachesTheMarkup(t *testing.T) {
	h := serve(t, fleet.ClusterView{Name: "tenant-01", Status: health.StatusWarn})
	if body := get(t, h, "/").Body.String(); !strings.Contains(body, `class="card warn"`) {
		t.Error("the card carries no severity class")
	}
}

// A row that cannot be read shows why, and shows no numbers at all. Counts
// under a connection error look current and are not.
func TestFleetPage_ProblemRowShowsNoCounts(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01",
		Status:  health.StatusErr,
		Problem: "read tenant-01-kubeconfig: secret not found",
	})
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "secret not found") {
		t.Error("the reason is not on the page")
	}
	for _, unwanted := range []string{"Control Plane", "Workers", "version unknown", `<div class="seen">`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an unreadable row still rendered %q", unwanted)
		}
	}
}

// An empty fleet says so rather than rendering a blank page that looks like a
// still-loading one.
func TestFleetPage_EmptyFleetSaysSo(t *testing.T) {
	if body := get(t, serve(t), "/").Body.String(); !strings.Contains(body, "No Cluster API clusters found") {
		t.Error("an empty fleet rendered nothing to explain itself")
	}
}

// A probe has no session, so gating it would make the deployment
// unschedulable.
func TestHealthz_IsNotGated(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{})}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("got %d", rec.Code)
	}
}

// The stream opens with the current state instead of waiting for the next
// change. A browser reconnecting after a blip would otherwise sit on stale
// markup until something in the fleet happened to move.
func TestEvents_FirstFrameIsImmediate(t *testing.T) {
	h := serve(t, fleet.ClusterView{Namespace: "capi", Name: "tenant-01", Status: health.StatusOK})

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, cancel := contextWithFrame(t)
	defer cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: fleet\n") {
		t.Fatalf("stream did not open with a fleet event:\n%s", firstLine(body))
	}
	if !strings.Contains(body, "tenant-01") {
		t.Error("first frame carries no cluster")
	}
	// Every line of the fragment needs its own data: prefix or the browser
	// silently drops the rest of the frame.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
			continue
		}
		t.Errorf("unprefixed line in the SSE frame: %q", line)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestAgo(t *testing.T) {
	ago := funcs()["ago"].(func(time.Time) string)
	for name, tc := range map[string]struct {
		in   time.Time
		want string
	}{
		"zero":    {time.Time{}, "never"},
		"seconds": {time.Now().Add(-30 * time.Second), "30s ago"},
		"minutes": {time.Now().Add(-5 * time.Minute), "5m ago"},
		"hours":   {time.Now().Add(-3 * time.Hour), "3h ago"},
	} {
		if got := ago(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

// One binnacle runs per management cluster and sites reuse workload cluster
// names, so several deployments render pages identical down to the names on the
// cards. The site has to reach both the header and the browser title, because a
// tab strip shows only the title.
func TestFleetPage_SiteNamesTheDeployment(t *testing.T) {
	body := get(t, serve(t, fleet.ClusterView{Name: "tenant-01"}), "/").Body.String()
	if !strings.Contains(body, "<title>site-a &middot; Binnacle</title>") {
		t.Error("the site is not in the browser title")
	}
	if !strings.Contains(body, `<span class="site">site-a</span>`) {
		t.Error("the site is not in the page header")
	}
}

// A single local instance has nothing to be confused with, so an unset site is
// absence rather than an empty badge.
func TestFleetPage_UnnamedSiteRendersNothingExtra(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{}, 1)}, auth.Open{}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/").Body.String()
	if !strings.Contains(body, "<title>Binnacle</title>") {
		t.Error("an unnamed instance should title plainly")
	}
	if strings.Contains(body, `class="site"`) {
		t.Error("an unnamed instance rendered an empty site badge")
	}
}

// A cluster that has not reported yet must not render zeros. They are numbers
// nobody has observed, and 0/0 in a replica column looks like a cluster with
// nothing in it rather than one we have not heard from — the milder form of
// the same mistake as showing counts under a connection error.
func TestFleetPage_UnreportedClusterShowsNoZeros(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-05",
		Status: health.StatusLoading,
		Cells:  []health.Cell{{Name: "CAPI", Status: health.StatusLoading}},
		// UpdatedAt deliberately zero: nothing has published.
	})
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "Waiting for the first report") {
		t.Error("an unreported cluster does not say so")
	}
	for _, unwanted := range []string{"0/0", "Control Plane", `<div class="seen">`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an unreported cluster rendered %q", unwanted)
		}
	}
	// Its cells still belong to it: "CAPI, still loading" is real information.
	if !strings.Contains(body, "CAPI") {
		t.Error("cells were dropped along with the counts")
	}
}

func serveDetail(t *testing.T, d fleet.ClusterDetail) http.Handler {
	t.Helper()
	s, err := New(&fakeFleet{
		detail:  map[string]fleet.ClusterDetail{d.Namespace + "/" + d.Name: d},
		changed: make(chan struct{}, 1),
	}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

// The detail page is where somebody sits during an upgrade. It has to carry
// what sextant's core panes carry, or there is no reason to open it.
func TestClusterPage_RendersTheCorePanes(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{
			Namespace: "capi", Name: "tenant-01", Version: "v1.36.2", Phase: "Provisioned",
			Status: health.StatusWarn,
			Cells:  []health.Cell{{Name: "Nodes", Status: health.StatusWarn, Detail: "1 cordoned"}},
			Pools: []fleet.NodePool{
				{Role: "Control Plane", Name: "tenant-01-kcp", Ready: 3, Desired: 3, Version: "v1.36.2"},
			},
			Nodes: fleet.NodeCount{Ready: 5, Total: 5}, NodesKnown: true,
			Summaries: []fleet.SummaryBlock{{Title: "Ceph", Lines: []string{"health  HEALTH_OK"}}},
			UpdatedAt: time.Now(),
		},
		Machines: fleet.Split[model.Machine]{Shown: []model.Machine{{Name: "tenant-01-machine-1", Phase: "Running",
			Version: "v1.36.2", NodeName: "node-1.site-a.example", Age: 31 * 24 * time.Hour}}},
		Hosts: fleet.Split[model.BareMetalHost]{Shown: []model.BareMetalHost{{Name: "host-1.site-a.example", State: "provisioned",
			OperationalStatus: "error", ErrorMessage: "timed out waiting for the deploy image"}}},
		NodeRows: fleet.Split[fleet.NodeRow]{Shown: []fleet.NodeRow{{
			Node:       model.Node{Name: "node-1.site-a.example", Role: "control-plane", Status: "Ready", Version: "v1.36.2"},
			CPUPercent: 28, MemPercent: 41,
		}}},
		UnhealthyPods: []model.Pod{{Namespace: "example-system", Name: "api-1",
			ReadyTotal: 1, Status: "CrashLoopBackOff", Restarts: 441}},
		Events: fleet.Split[fleet.EventGroup]{Shown: fleet.GroupEvents([]model.Event{
			{Type: "Warning", Reason: "Unhealthy", ObjectKind: "Pod",
				ObjectName: "api-1", Message: "Readiness probe failed", Count: 9148,
				LastTimestamp: time.Now().Add(-time.Minute)},
			// Two objects reporting one policy violation: the row should name
			// the kind and the tally, not either object.
			{Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
				ObjectName: "batch-1", Message: "HostPath volumes are forbidden", Count: 3,
				LastTimestamp: time.Now().Add(-2 * time.Minute)},
			{Type: "Warning", Reason: "PolicyViolation", ObjectKind: "ReplicaSet",
				ObjectName: "batch-2", Message: "HostPath volumes are forbidden", Count: 4,
				LastTimestamp: time.Now().Add(-3 * time.Minute)},
		})},
		EventsTotal: 9155,
	}

	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	for _, want := range []string{
		"tenant-01", "v1.36.2", "Node pools", "tenant-01-kcp",
		"Machines &amp; hosts", "tenant-01-machine-1",
		"host-1.site-a.example", "timed out waiting for the deploy image",
		"Nodes", "node-1.site-a.example", "28%", "41%",
		"Unhealthy pods", "CrashLoopBackOff", "441",
		"Events", "Readiness probe failed", "9148",
		// Grouped: one row for the two replica sets, counting both.
		"HostPath volumes are forbidden", "&times;2 objects", "7",
		"2 groups", "9155 events",
		"Subsystems", "HEALTH_OK",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cluster page does not mention %q", want)
		}
	}
}

// A cluster that is not tracked is a 404. Rendering an empty page would claim
// the cluster exists and has nothing in it, which is a different statement.
func TestClusterPage_UnknownClusterIs404(t *testing.T) {
	h := serveDetail(t, fleet.ClusterDetail{ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01"}})
	if rec := get(t, h, "/cluster/capi/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
	if rec := get(t, h, "/cluster/other/tenant-01"); rec.Code != http.StatusNotFound {
		t.Errorf("wrong namespace: got %d, want 404", rec.Code)
	}
}

// Truncation has to be visible: "60 unhealthy pods" and "60 of 400" are
// different situations, and a page that silently drops the tail reports the
// first when it means the second.
func TestClusterPage_TruncationIsStated(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView:   fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		UnhealthyPods: []model.Pod{{Namespace: "ns", Name: "a", Status: "Error"}},
		PodsTruncated: 339,
	}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	if !strings.Contains(body, "of 340") || !strings.Contains(body, "339 more not shown") {
		t.Error("the page does not say what it left out")
	}
}

// Each fleet card links to its own cluster.
func TestFleetPage_CardsLinkToTheCluster(t *testing.T) {
	h := serve(t, fleet.ClusterView{Namespace: "capi", Name: "tenant-01", Status: health.StatusOK})
	if body := get(t, h, "/").Body.String(); !strings.Contains(body, `href="/cluster/capi/tenant-01"`) {
		t.Error("the card does not link to its cluster")
	}
}

// The cluster page streams its own body, so a reader watching an upgrade never
// has to wonder whether the tab is stale.
func TestClusterEvents_StreamsTheBody(t *testing.T) {
	d := fleet.ClusterDetail{ClusterView: fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", Version: "v1.36.2", NodesKnown: true,
	}}
	req := httptest.NewRequest(http.MethodGet, "/cluster/capi/tenant-01/events", nil)
	ctx, cancel := contextWithFrame(t)
	defer cancel()
	rec := httptest.NewRecorder()
	serveDetail(t, d).ServeHTTP(rec, req.WithContext(ctx))

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: cluster\n") {
		t.Fatalf("stream did not open with a cluster event:\n%s", firstLine(body))
	}
	if !strings.Contains(body, "v1.36.2") {
		t.Error("first frame carries no cluster detail")
	}
}

// The network pane carries what sextant's own network pane carries, and each
// subsystem appears only when it published something.
func TestClusterPage_NetworkPane(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Subsystems: fleet.Subsystems{
			Cilium: &cilium.State{
				Tier: subsystem.TierFull, AgentsReady: 5, AgentsDesired: 5,
				Rollout: subsystem.Rollout{Desired: 5, Updated: 5},
				Status: cilium.Status{
					// Cilium's own raw value, deliberately: "true" is what the
					// agent reports when it has *replaced* kube-proxy.
					Version: "1.19.7", KubeProxyReplacement: "true",
					IPAM: cilium.IPAM{Used: 130, Available: 124},
				},
			},
			MetalLB: &metallb.State{
				SpeakerReady: 4, SpeakerDesired: 4,
				Pools: []metallb.Pool{
					{Name: "unannounced", Addresses: []string{"192.0.2.12-192.0.2.99"}},
				},
			},
		},
	}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	for _, want := range []string{"Network", "Cilium", "1.19.7", "MetalLB", "unannounced", "192.0.2.12"} {
		if !strings.Contains(body, want) {
			t.Errorf("network pane does not mention %q", want)
		}
	}
	// Cilium reports "true" when it has replaced kube-proxy, so the raw field
	// says the opposite of the truth. The page must render sextant's reading of
	// it, never the field.
	if !strings.Contains(body, "replaced by Cilium") {
		t.Error("the kube-proxy mode was not interpreted")
	}
	if strings.Contains(body, ">kube-proxy</td><td colspan=\"2\">true<") {
		t.Error("the raw kube-proxy value reached the page")
	}

	// A pool nothing advertises hands out addresses that never get announced.
	// It is silent otherwise, which is exactly why it is called out.
	if !strings.Contains(body, "nothing announces") {
		t.Error("an unadvertised pool was not flagged")
	}
	// OVN published nothing, so it gets no section at all.
	if strings.Contains(body, ">OVN<") {
		t.Error("an absent subsystem rendered a section")
	}
}

// The cloud pane shows drains first: mid-maintenance that is the question, and
// a migration table only answers it sideways.
func TestClusterPage_CloudPane(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Subsystems: fleet.Subsystems{
			OpenStack: &openstack.State{
				Services: []openstack.ServiceSummary{{Service: "compute", Total: 8, Up: 7}},
			},
			Inventory: &openstack.Inventory{
				Counts: []openstack.Count{
					{Label: "Servers", Total: 54},
					{Label: "Load Balancers", Absent: true},
				},
			},
		},
		Drains: []openstack.Drain{{Host: "compute-node-7.site-a.example", Remaining: 2, Stuck: 2}},
	}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	for _, want := range []string{"Cloud", "Draining hosts", "compute-node-7.site-a.example", "compute", "7/8", "Servers", "54"} {
		if !strings.Contains(body, want) {
			t.Errorf("cloud pane does not mention %q", want)
		}
	}
	// A cloud with no Octavia is correctly configured, not broken.
	if !strings.Contains(body, "not deployed") {
		t.Error("an absent service was not distinguished from a failed one")
	}
}

// Neither pane appears on a cluster running none of it.
func TestClusterPage_NoSubsystemsNoPanes(t *testing.T) {
	d := fleet.ClusterDetail{ClusterView: fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", NodesKnown: true,
	}}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	for _, unwanted := range []string{"<h3>Network</h3>", "<h3>Cloud</h3>"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("rendered %q for a cluster running none of it", unwanted)
		}
	}
}

// A subsystem reporting below full detail says so on the page, rather than
// rendering thin and letting a reader assume there is nothing to report.
func TestClusterPage_ReducedTierIsStated(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Subsystems: fleet.Subsystems{Cilium: &cilium.State{
			Tier: subsystem.TierInformer, TierReason: "no permission to exec into cilium pods",
		}},
	}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	if !strings.Contains(body, "below full detail") || !strings.Contains(body, "no permission to exec") {
		t.Error("a reduced tier rendered thin without saying why")
	}
}

// A production-sized cluster folds its quiet rows away, and the rows that
// matter stay on the page. The whole point of the fold is that it never hides
// a problem, so the assertions are about what survives it.
func TestDetail_FoldsQuietRowsButKeepsProblems(t *testing.T) {
	// The split itself is decided and tested in the fleet package; what this
	// asserts is that the template renders both halves and puts them in the
	// right place.
	var quiet []fleet.NodeRow
	for i := 0; i < 60; i++ {
		quiet = append(quiet, fleet.NodeRow{Node: model.Node{
			Name: fmt.Sprintf("compute-%02d.site-a.example", i), Role: "compute", Status: "Ready",
		}})
	}
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "big", NodesKnown: true},
		NodeRows: fleet.Split[fleet.NodeRow]{
			Shown: []fleet.NodeRow{{Node: model.Node{
				Name: "broken.site-a.example", Role: "compute", Status: "NotReady",
			}}},
			Quiet: quiet,
		},
	}

	body := get(t, serveDetail(t, d), "/cluster/capi/big").Body.String()
	for _, want := range []string{
		"broken.site-a.example", // the problem is on the page
		"<details",              // the rest is behind a disclosure
		"60 nodes Ready and schedulable",
		"compute-59.site-a.example", // folded, not dropped
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	// The problem must not itself be inside the disclosure.
	if i, j := strings.Index(body, "broken.site-a.example"), strings.Index(body, "<details"); i > j {
		t.Error("the NotReady node was rendered inside the fold")
	}
}

// The expanded tier is markup the container query reveals, so it has to be in
// the response whether or not a given viewer's card is wide enough for it.
func TestFleetPage_CarriesTheExpandedTier(t *testing.T) {
	d := fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", NodesKnown: true,
		Nodes:         fleet.NodeCount{Ready: 69, Total: 70},
		UnhealthyPods: 12,
		NodesByRole: []fleet.RoleCount{
			{Role: "compute", Ready: 61, Total: 62},
			{Role: "control-plane", Ready: 3, Total: 3},
		},
		TopUnhealthyPods: []fleet.PodRef{
			{Namespace: "openstack", Name: "octavia-api", Status: "CrashLoopBackOff", Restarts: 441},
		},
		Summaries: []fleet.SummaryBlock{{Title: "Ceph", Lines: []string{"health  HEALTH_WARN"}}},
	}

	body := get(t, serve(t, d), "/").Body.String()
	for _, want := range []string{
		"compute", "61/62", "control-plane", "3/3", // nodes by role
		"openstack/octavia-api", "CrashLoopBackOff", // named pods
		"and 11 more",                         // the count is not capped even though the names are
		"HEALTH_WARN",                         // the subsystem headline
		`href="/cluster/capi/tenant-01#pods"`, // the deep link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fleet page is missing %q", want)
		}
	}
}

// Wall mode is opt-in and must not leak into an ordinary page load.
func TestFleetPage_WallModeIsOptIn(t *testing.T) {
	d := fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true}
	srv := serve(t, d)

	if body := get(t, srv, "/").Body.String(); strings.Contains(body, `class="wall"`) {
		t.Error("an ordinary page load came back in wall mode")
	}
	if body := get(t, srv, "/?display=wall").Body.String(); !strings.Contains(body, `class="wall"`) {
		t.Error("?display=wall did not scale the page")
	}
	// Any other value is not wall mode, rather than an error.
	if body := get(t, srv, "/?display=desk").Body.String(); strings.Contains(body, `class="wall"`) {
		t.Error("an unrecognized display mode was treated as wall")
	}
}

// The first row has to fill whether or not a cluster reports subsystems: two
// quarters and a half, or two halves — never a pane holding a gap beside it.
func TestClusterPage_FirstRowFillsEitherWay(t *testing.T) {
	base := fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", NodesKnown: true,
		Nodes: fleet.NodeCount{Ready: 3, Total: 3},
	}

	withCeph := base
	withCeph.Summaries = []fleet.SummaryBlock{{Title: "Ceph", Lines: []string{"health  HEALTH_OK"}}}
	body := get(t, serveDetail(t, fleet.ClusterDetail{ClusterView: withCeph}), "/cluster/capi/tenant-01").Body.String()
	// The closing bracket matters: without it this also counts the pods pane,
	// whose attribute begins with the same text.
	if n := strings.Count(body, `class="pane narrow">`); n != 2 {
		t.Errorf(`got %d "pane narrow" sections, want 2 (node pools, subsystems)`, n)
	}
	// The pods pane keeps the remaining half either way: it is the only one of
	// the three rendering a namespaced pod name against a qualified node name.
	if !strings.Contains(body, `class="pane half" id="pods"`) {
		t.Error("the pods pane did not take the rest of the row beside the narrow panes")
	}

	body = get(t, serveDetail(t, fleet.ClusterDetail{ClusterView: base}), "/cluster/capi/tenant-01").Body.String()
	if strings.Contains(body, `class="pane narrow"`) {
		t.Error("without subsystems the row should be two halves, not a quarter and a gap")
	}
	if !strings.Contains(body, `class="pane half" id="pods"`) {
		t.Error("the pods pane did not widen to a half when subsystems were absent")
	}
}

// What is broken belongs where a reader lands, not below five healthy panes.
func TestClusterPage_ProblemsComeBeforeInventory(t *testing.T) {
	d := fleet.ClusterDetail{ClusterView: fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", NodesKnown: true,
	}}
	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()
	pods := strings.Index(body, `id="pods"`)
	machines := strings.Index(body, "Machines &amp; hosts")
	events := strings.Index(body, "<h3>Events")
	if pods < 0 || machines < 0 || events < 0 {
		t.Fatal("a pane is missing from the page")
	}
	if pods > machines {
		t.Error("unhealthy pods rendered below the machine inventory")
	}
	// Machines & hosts and Nodes share a row: both pack to about a quarter of a
	// full-width pane, so neither earns one.
	if !strings.Contains(body, `<section class="pane half">`) {
		t.Error("the machines pane did not take a half")
	}
	if !strings.Contains(body, `<section class="pane half" id="nodes">`) {
		t.Error("the nodes pane did not take a half")
	}
	if machines > events {
		t.Error("events rendered above the inventory")
	}
}

// Tables of short columns are sized to their content. Stretching them across
// the pane spreads the slack between the columns; handing one column 99% moves
// the same slack inside it and leaves a node name eighteen hundred pixels from
// its role. Only the events table, which has a genuinely unbounded message
// column, should fill the width.
func TestClusterPage_IdentifierTablesArePacked(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{
			Namespace: "capi", Name: "tenant-01", NodesKnown: true,
			Pools: []fleet.NodePool{{Role: "Control Plane", Name: "tenant-01-kcp", Ready: 3, Desired: 3}},
		},
		Machines: fleet.Split[model.Machine]{Shown: []model.Machine{{Name: "m-1", Phase: "Running"}}},
		Hosts:    fleet.Split[model.BareMetalHost]{Shown: []model.BareMetalHost{{Name: "h-1", State: "provisioned"}}},
		NodeRows: fleet.Split[fleet.NodeRow]{Shown: []fleet.NodeRow{{
			Node: model.Node{Name: "node-1.site-a.example", Role: "control-plane", Status: "Ready"},
		}}},
		UnhealthyPods: []model.Pod{{Namespace: "ns", Name: "api-1", Status: "CrashLoopBackOff"}},
		Events: fleet.Split[fleet.EventGroup]{Shown: fleet.GroupEvents([]model.Event{{
			Type: "Warning", Reason: "Unhealthy", ObjectKind: "Pod", ObjectName: "api-1",
			Message: "probe failed", LastTimestamp: time.Now(),
		}})},
	}

	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()

	// Node pools, machines, hosts, nodes, unhealthy pods.
	if n := strings.Count(body, `<table class="packed">`); n != 5 {
		t.Errorf("got %d packed tables, want 5", n)
	}
	// The growth columns that caused the regression must not come back.
	for _, gone := range []string{`class="grow"`, `class="grow-2"`, `grow-2">`} {
		if strings.Contains(body, gone) {
			t.Errorf("a growth column class is back on the page: %s", gone)
		}
	}
	// Events still fills, and still claims its message column.
	if !strings.Contains(body, `<td class="msg">probe failed</td>`) {
		t.Error("the events message column lost its growth claim")
	}
}
