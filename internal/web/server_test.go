package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/subsystem"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
)

type fakeFleet struct {
	clusters []fleet.ClusterView
	detail   map[string]fleet.ClusterDetail
	storage  fleet.Storage
	changed  chan struct{}
}

func (f *fakeFleet) View() []fleet.ClusterView { return f.clusters }
func (f *fakeFleet) Storage() fleet.Storage    { return f.storage }
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
			UpdatedAt: time.Now(),
		},
		// Ceph renders from its typed state now, not from the plugin's summary
		// block: see the ceph-status template.
		Subsystems: fleet.Subsystems{Ceph: &ceph.State{Status: ceph.Status{
			Health: "HEALTH_OK",
			Mons:   ceph.Mons{Total: 3, InQuorum: 3},
			OSDs:   ceph.OSDs{Total: 36, Up: 36, In: 36},
			PGs: ceph.PGs{Total: 1953, Pools: 14,
				ByState:   []ceph.PGState{{Name: "active+clean", Count: 1953}},
				UsedBytes: 13, TotalBytes: 100},
		}}},
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

// Nine load balancers in ERROR was the most alarming fact on the cloud pane and
// rendered in the same grey as a project count. The order matters too: a
// template iterating the map sorts by key, which put ACTIVE first.
func TestClusterPage_FailingCloudStatesAreLegible(t *testing.T) {
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Subsystems: fleet.Subsystems{Inventory: &openstack.Inventory{Counts: []openstack.Count{
			{Label: "Load Balancers", Total: 12, ByState: map[string]int{"ACTIVE": 3, "ERROR": 9}},
		}}},
	}

	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()

	if !strings.Contains(body, `<span class="state errish">ERROR 9</span>`) {
		t.Errorf("the ERROR state is not marked as one:\n%s", body)
	}
	if !strings.Contains(body, `<span class="state">ACTIVE 3</span>`) {
		t.Errorf("a settled state should render plain:\n%s", body)
	}
	// Most common first, so the bulk state leads — and each pair is one span, so
	// a wrap cannot separate a state from its count.
	if i, j := strings.Index(body, "ERROR 9"), strings.Index(body, "ACTIVE 3"); i > j {
		t.Error("ACTIVE 3 came before ERROR 9: the map order leaked through")
	}
}

// The same guard for the fleet page: every card branch and the storage panel,
// rendered with data in each.
func TestFleetPage_EveryCardBranchRenders(t *testing.T) {
	rich := richDetail().ClusterView
	rich.NodesByRole = []fleet.RoleCount{{Role: "control-plane", Ready: 2, Total: 3}}
	rich.TopUnhealthyPods = []fleet.PodRef{{Namespace: "ns", Name: "api-1",
		Status: "CrashLoopBackOff", Restarts: 441}}
	rich.UnhealthyPods = 8
	rich.WorkloadProblem = "context deadline exceeded"
	rich.CloudsProblem = "no such cloud entry"

	// And one card that could not be read at all, which takes a different path.
	broken := fleet.ClusterView{Namespace: "capi", Name: "tenant-02", Problem: "unreachable"}

	st := fleet.StorageFor([]model.BareMetalHost{
		{Namespace: "machines", Name: "r0102-01-cephmon", State: "provisioned",
			OperationalStatus: "error", ErrorMessage: "deploy image timed out",
			Labels: map[string]string{fleet.LabelRole: "cephmon", fleet.LabelClusterID: "fsid"}},
	}, []fleet.CephReport{{
		Cluster: fleet.ClusterRef{Namespace: "capi", Name: "tenant-01"},
		Status:  ceph.Status{FSID: "fsid", Health: "HEALTH_WARN"},
	}})

	s, err := New(&fakeFleet{
		clusters: []fleet.ClusterView{rich, broken}, storage: st,
		changed: make(chan struct{}, 1),
	}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, s.Handler(), "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "can't evaluate field") {
		t.Errorf("template error in the body:\n%s", body)
	}
}

// The events table sizes to its content rather than starting at the full width.
//
// The growth column put a thousand pixels between a short message and the count
// at the pane's edge, while holding the object column at its floor so a pod name
// wrapped. Both are the same cause.
func TestClusterPage_EventsTableFitsItsContent(t *testing.T) {
	body := get(t, serveDetail(t, richDetail()), "/cluster/capi/tenant-01").Body.String()

	if !strings.Contains(body, `<table class="fit">`) {
		t.Error("the events table does not size to its content")
	}
	// And nothing else adopted it by accident: .fit is wrong for a table whose
	// columns should share the pane.
	if n := strings.Count(body, `class="fit"`); n != 1 {
		t.Errorf("%d tables are sized to content, want only the events table", n)
	}
}

// Ceph renders from its typed state, not from the plugin's summary block.
//
// The block is pre-formatted text — three lines with the columns set in spaces
// — and pre-formatted text cannot wrap, spread or share a row, so it read as
// cramped at every pane width it was given. The fields carry the same
// judgements through exported methods, so laying them out here is presentation
// rather than a second opinion.
func TestCephRendersFromItsFields(t *testing.T) {
	d := richDetail()
	d.Subsystems.Ceph = &ceph.State{Status: ceph.Status{
		Health: "HEALTH_WARN",
		Mons:   ceph.Mons{Total: 3, InQuorum: 2},
		OSDs:   ceph.OSDs{Total: 36, Up: 35, In: 36},
		PGs: ceph.PGs{Total: 1953, Pools: 14,
			ByState:   []ceph.PGState{{Name: "active+clean", Count: 1950}},
			UsedBytes: 13, TotalBytes: 100},
		Checks: []ceph.Check{{Name: "OSD_DOWN", Message: "1 osds down"}},
	}}
	// A plugin with no public types still gets its block rendered.
	d.Summaries = []fleet.SummaryBlock{{Title: "Widget", Lines: []string{"widgets  fine"}}}

	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()

	for _, want := range []string{
		"HEALTH_WARN", "2/3", "35/36 up", "13% used", "1950/1953 clean", "OSD_DOWN",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Ceph pane is missing %q", want)
		}
	}
	// Each figure carries its own verdict, so a healthy total does not hide an
	// unhealthy part.
	if n := strings.Count(body, `class="v warnish"`); n < 3 {
		t.Errorf("only %d Ceph figures are marked unhealthy; health, mons, osds and pgs all are", n)
	}
	// The other plugin's block is untouched, and Ceph's is not rendered twice.
	if !strings.Contains(body, "widgets  fine") {
		t.Error("a plugin's own summary block was dropped")
	}
	if strings.Contains(body, `<pre class="summary">health`) {
		t.Error("Ceph rendered through the summary block as well as its fields")
	}
}

// Every disclosure carries an id, because that is how its open state survives a
// pushed fragment. Without one the reader's click is undone within a second by
// the next update, which is what this test exists to stop regressing.
func TestDisclosuresCarryIdentifiers(t *testing.T) {
	pages := map[string]string{
		"cluster": get(t, serveDetail(t, foldedDetail()), "/cluster/capi/tenant-01").Body.String(),
		"fleet":   fleetPageWithFoldedStorage(t),
	}

	for name, body := range pages {
		count := strings.Count(body, "<details")
		if count == 0 {
			t.Fatalf("%s page rendered no disclosures to check", name)
		}
		// Every <details> opens with its class and then its id.
		if withID := strings.Count(body, `<details class="quiet" id="`); withID != count {
			t.Errorf("%s page: %d of %d disclosures carry an id", name, withID, count)
		}
	}

	// And the page ships the script that restores them.
	if !strings.Contains(pages["cluster"], "binnacleKeepOpen") {
		t.Error("the cluster page does not restore open disclosures")
	}
	if !strings.Contains(pages["fleet"], "binnacleKeepOpen") {
		t.Error("the fleet page does not restore open disclosures")
	}
}

// foldedDetail is a cluster big enough that every table folds its quiet rows.
func foldedDetail() fleet.ClusterDetail {
	d := richDetail()
	var machines []model.Machine
	var hosts []model.BareMetalHost
	var nodes []fleet.NodeRow
	var events []fleet.EventGroup
	for i := 0; i < 12; i++ {
		n := strconv.Itoa(i)
		machines = append(machines, model.Machine{Name: "m-" + n, Phase: "Running"})
		hosts = append(hosts, model.BareMetalHost{Name: "h-" + n, State: "provisioned", OperationalStatus: "OK"})
		nodes = append(nodes, fleet.NodeRow{Node: model.Node{Name: "n-" + n, Status: "Ready"}})
		events = append(events, fleet.EventGroup{
			Event:       model.Event{Type: "Normal", Reason: "Pulled", ObjectKind: "Pod", Message: "pulled " + n},
			Occurrences: 1, Objects: 1,
		})
	}
	d.Machines = fleet.Split[model.Machine]{Quiet: machines}
	d.Hosts = fleet.Split[model.BareMetalHost]{Quiet: hosts}
	d.NodeRows = fleet.Split[fleet.NodeRow]{Quiet: nodes}
	d.Events = fleet.Split[fleet.EventGroup]{Quiet: events}
	return d
}

// fleetPageWithFoldedStorage renders a storage panel whose host table folds.
func fleetPageWithFoldedStorage(t *testing.T) string {
	t.Helper()
	var hosts []model.BareMetalHost
	for i := 0; i < 12; i++ {
		hosts = append(hosts, model.BareMetalHost{
			Namespace: "machines", Name: "cephosd-" + strconv.Itoa(i),
			State: "provisioned", OperationalStatus: "OK",
			Labels: map[string]string{fleet.LabelRole: "cephosd", fleet.LabelClusterID: "fsid"},
		})
	}
	s, err := New(&fakeFleet{
		storage: fleet.StorageFor(hosts, nil), changed: make(chan struct{}, 1),
	}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return get(t, s.Handler(), "/").Body.String()
}

// A grid table declares its column count in a class, and nothing in the markup
// checks it: get it wrong and every cell after the first row lands in the wrong
// column, on a page that still renders. This counts the headers and compares.
func TestPackedTablesDeclareTheirRealColumnCount(t *testing.T) {
	pages := []string{
		get(t, serveDetail(t, richDetail()), "/cluster/capi/tenant-01").Body.String(),
		fleetPageWithStorage(t),
	}

	seen := 0
	for _, body := range pages {
		for _, table := range packedTables(body) {
			seen++
			head, _, ok := strings.Cut(table.html, "</thead>")
			if !ok {
				t.Errorf("packed table with cols-%d has no thead", table.cols)
				continue
			}
			// "<th" alone also counts the <thead that opens the block.
			n := strings.Count(head, "<th>") + strings.Count(head, "<th ")
			if n != table.cols {
				t.Errorf("table declares cols-%d but has %d headers: %.120s", table.cols, n, table.html)
			}
		}
	}
	if seen < 6 {
		t.Errorf("only checked %d packed tables; the pages should render more", seen)
	}
}

// richDetail is a cluster with every table, pane and subsystem populated.
//
// It exists because of a 500 on a live page. html/template resolves a field
// only when data reaches it, so a pane nothing has ever populated is a pane
// nothing has ever checked: the migrations table referred to a
// `.ServerID` that has never existed on openstack.Migration, and it sat there
// from the day the cloud pane was written until a cluster finally had a
// migration to render. The whole page 500s, not the table.
//
// So: every branch gets data. A field renamed in a sextant release, or misread
// when it was written, fails here instead of on somebody's screen.
func richDetail() fleet.ClusterDetail {
	now := time.Now()
	return fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{
			Namespace: "capi", Name: "tenant-01", NodesKnown: true,
			Version: "v1.36.2", Phase: "Provisioned", Paused: true,
			Pools: []fleet.NodePool{{Name: "tenant-01-kcp", Role: "control-plane",
				Ready: 3, Desired: 3, Version: "v1.36.2", Rolling: true}},
			Nodes:     fleet.NodeCount{Ready: 6, Total: 6, Cordoned: 2},
			Capacity:  fleet.Capacity{CPURequested: 2400, CPUAllocatable: 10000, MemRequested: 35, MemAllocatable: 100},
			Workloads: []fleet.WorkloadCount{{Kind: "Deployment", Ready: 151, Total: 152}},
			Summaries: []fleet.SummaryBlock{{Title: "Ceph", Lines: []string{"health  HEALTH_WARN"}}},
		},
		Machines: fleet.Split[model.Machine]{Shown: []model.Machine{
			{Name: "tenant-01-kcp-abc", Phase: "Running", Version: "v1.36.2"}}},
		Hosts: fleet.Split[model.BareMetalHost]{Shown: []model.BareMetalHost{
			{Name: "host-1", State: "provisioned", OperationalStatus: "error",
				ErrorMessage: "timed out waiting for the deploy image"}}},
		HostsElsewhere: 9,
		NodeRows: fleet.Split[fleet.NodeRow]{Shown: []fleet.NodeRow{{
			Node: model.Node{Name: "node-1.site-a.example", Role: "control-plane", Status: "Ready"}}}},
		UnhealthyPods: []model.Pod{{Namespace: "ns", Name: "api-1", Status: "CrashLoopBackOff"}},
		PodsTruncated: 4,
		Events: fleet.Split[fleet.EventGroup]{Shown: fleet.GroupEvents([]model.Event{{
			Type: "Warning", Reason: "DrainFailed", ObjectKind: "BareMetalHost",
			ObjectName: "host-1", Message: "failed to migrate virtual routers",
			LastTimestamp: now}})},
		EventsElsewhere: 45,
		Subsystems: fleet.Subsystems{
			// Reduced tiers, so the "reporting below full detail" pane renders.
			Cilium: &cilium.State{
				Tier: subsystem.TierInformer, TierReason: "pods/exec denied",
				AgentsReady: 5, AgentsDesired: 6, Pod: "cilium-abc",
				Rollout: subsystem.Rollout{Desired: 6, Updated: 5, Manual: true},
				Status: cilium.Status{
					Version: "1.19.7", State: "Ok", KubeProxyReplacement: "true",
					EncryptionMode: "Ztunnel",
					IPAM:           cilium.IPAM{Used: 133, Available: 121},
					Controllers:    cilium.Controllers{Failing: 1, Total: 40},
					Unreadable:     []string{"encryption"},
				}},
			OVN: &ovn.State{
				Statuses: []ovn.ClusterStatus{{
					Database: "OVN_Northbound", Role: "leader", Term: 389,
					Leader: "ovn-ovsdb-nb-1", Servers: []ovn.Server{{Name: "nb-1"}}}},
				Components: []ovn.Component{{
					Name: "ovn-northd", Rollout: subsystem.Rollout{Desired: 3, Updated: 3, Ready: 3}}},
			},
			MetalLB: &metallb.State{
				Pools: []metallb.Pool{
					{Name: "default", Addresses: []string{"192.0.2.12-192.0.2.99"},
						Advertised: []string{"L2"}, Assigned: 8, Available: 80},
					// No Advertised, so UnadvertisedPools() has something to
					// report and the halfblind line renders.
					{Name: "spare", Addresses: []string{"192.0.2.200-192.0.2.210"}},
				},
				// A service with no external IP is what PendingServices() counts.
				Services:     []metallb.Service{{Namespace: "ns", Name: "waiting"}},
				SpeakerReady: 4, SpeakerDesired: 4,
			},
			OpenStack: &openstack.State{Services: []openstack.ServiceSummary{
				{Service: "compute", Up: 7, Total: 8, Disabled: 1}}},
			Inventory: &openstack.Inventory{Counts: []openstack.Count{
				{Label: "Servers", Total: 50, ByState: map[string]int{"ACTIVE": 47, "ERROR": 3}},
				{Label: "Load Balancers", Absent: true},
				{Label: "Volumes", Err: errors.New("timed out")},
			}},
		},
		// The pane that 500'd the page: a drain in progress and a migration to
		// show for it.
		Drains: []openstack.Drain{{Host: "compute-1", Remaining: 4, Moving: 0, Stuck: 2}},
		Shown: openstack.Shown{Rows: []openstack.Migration{{
			ID: 70551, Status: "error", Type: "live-migration",
			InstanceUUID:  "3f2b1c8a-1111-2222-3333-444455556666",
			SourceCompute: "compute-1", DestCompute: "compute-2", UpdatedAt: now,
		}}},
	}
}

// Every pane on the cluster page renders without a template error.
//
// render buffers, so a bad field reference anywhere is a 500 for the whole page
// rather than one broken table. That makes this the cheapest test on the page
// and the one that would have caught the .ServerID bug on the day it was
// written.
func TestClusterPage_EveryPaneRenders(t *testing.T) {
	rec := get(t, serveDetail(t, richDetail()), "/cluster/capi/tenant-01")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A template execution failure is reported as text, so check for its shape
	// as well as the status: "can't evaluate field X in type Y".
	if strings.Contains(body, "can't evaluate field") || strings.Contains(body, "executing \"") {
		t.Errorf("template error in the body:\n%s", body)
	}
	// Spot-check the pane that was broken, including the shortened UUID.
	for _, want := range []string{"Server migrations", "3f2b1c8a", "Draining hosts", "compute-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from a fully populated page", want)
		}
	}
	// Short in the cell, whole in the title: eight characters are what anybody
	// reads, and the rest is still there to copy.
	if !strings.Contains(body, `>3f2b1c8a</td>`) {
		t.Error("the cell does not hold the shortened UUID")
	}
	if !strings.Contains(body, `title="3f2b1c8a-1111-2222-3333-444455556666"`) {
		t.Error("the whole UUID is not recoverable from the row")
	}
}

// fleetPageWithStorage renders the fleet page with a storage panel on it.
func fleetPageWithStorage(t *testing.T) string {
	t.Helper()
	st := fleet.StorageFor([]model.BareMetalHost{
		{Namespace: "machines", Name: "r0102-01-cephmon", State: "provisioned", OperationalStatus: "OK",
			Labels: map[string]string{fleet.LabelRole: "cephmon", fleet.LabelClusterID: "fsid"}},
	}, nil)
	s, err := New(&fakeFleet{storage: st, changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return get(t, s.Handler(), "/").Body.String()
}

type packedTable struct {
	cols int
	html string
}

// packedTables finds every grid table on a page and the column count it claims.
func packedTables(body string) []packedTable {
	var out []packedTable
	const marker = `<table class="packed cols-`
	for rest := body; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(marker):]
		// The class may carry modifiers after the count ("cols-6 spread"), so
		// take the leading digits rather than everything up to the quote.
		attr, after, ok := strings.Cut(rest, `"`)
		if !ok {
			return out
		}
		digits := attr
		if i := strings.IndexFunc(attr, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = attr[:i]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return out
		}
		html, _, _ := strings.Cut(after, "</table>")
		out = append(out, packedTable{cols: n, html: html})
		rest = after
	}
}

// The storage panel renders every branch it has, because a template referring
// to a field the page data does not carry fails at execution — a 500 on the
// fleet page, and only on the sites that have the data to reach that branch.
func TestFleetPage_StoragePanel(t *testing.T) {
	labeled := fleet.StorageFor([]model.BareMetalHost{
		{Namespace: "machines", Name: "r0102-01-cephmon", State: "provisioned", OperationalStatus: "OK",
			Labels: map[string]string{fleet.LabelRole: "cephmon", fleet.LabelClusterID: "fsid-known"}},
	}, []fleet.CephReport{{
		Cluster: fleet.ClusterRef{Namespace: "capi", Name: "tenant-01"},
		Status: ceph.Status{FSID: "fsid-known", Health: "HEALTH_WARN",
			Mons: ceph.Mons{Total: 3, InQuorum: 2}, OSDs: ceph.OSDs{Total: 36, Up: 35, In: 36},
			Checks: []ceph.Check{{Name: "OSD_DOWN", Message: "1 osds down"}}},
	}})

	s, err := New(&fakeFleet{
		clusters: []fleet.ClusterView{{Namespace: "capi", Name: "tenant-01"}},
		storage:  labeled,
		changed:  make(chan struct{}, 1),
	}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/").Body.String()

	for _, want := range []string{
		"fsid-kno",                           // the fsid prefix names it
		`<a href="/cluster/capi/tenant-01">`, // and the reporter links back
		"HEALTH_WARN",                        // Ceph's own verdict
		"OSD_DOWN",                           // and its checks
		"r0102-01-cephmon",                   // the hardware
		"cephmon</span> 1/1",                 // per-role readiness
	} {
		if !strings.Contains(body, want) {
			t.Errorf("storage panel is missing %q", want)
		}
	}
	// Degraded by Ceph's own verdict, with every host provisioned cleanly.
	if !strings.Contains(body, "storage-pane warn") {
		t.Error("a HEALTH_WARN Ceph did not mark the pane degraded")
	}
}

// Hardware labeled for a Ceph nobody reports must say so. An absent health
// block on its own reads as good news.
func TestFleetPage_StoragePanelSaysWhenNobodyReports(t *testing.T) {
	silent := fleet.StorageFor([]model.BareMetalHost{
		{Namespace: "machines", Name: "r0306-01-cephosd", State: "provisioned", OperationalStatus: "OK",
			Labels: map[string]string{fleet.LabelRole: "cephosd", fleet.LabelClusterID: "fsid-orphan"}},
	}, nil)

	s, err := New(&fakeFleet{storage: silent, changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/").Body.String()

	if !strings.Contains(body, "No cluster in this fleet reports this Ceph") {
		t.Errorf("silence is not reported:\n%s", body)
	}
	if !strings.Contains(body, "storage-pane unknown") {
		t.Error("an unreported Ceph should not render as healthy")
	}
}

// A datacenter whose hardware is not labeled still has a storage layer, and the
// clusters still report it. The panel has to work from the report alone.
func TestFleetPage_StoragePanelWithoutLabeledHardware(t *testing.T) {
	unlabeled := fleet.StorageFor(nil, []fleet.CephReport{{
		Cluster: fleet.ClusterRef{Namespace: "capi", Name: "tenant-01"},
		Status:  ceph.Status{FSID: "fsid-only-reported", Health: "HEALTH_OK"},
	}})

	s, err := New(&fakeFleet{storage: unlabeled, changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/").Body.String()

	if !strings.Contains(body, "hardware not identified") {
		t.Errorf("a Ceph with no labeled hosts does not say so:\n%s", body)
	}
	if !strings.Contains(body, "HEALTH_OK") {
		t.Error("the reported health was dropped with the hardware")
	}
}

// A cluster with no warnings should say so once, not twice. The disclosure
// carries the good news, so there is no separate sentence above it.
func TestClusterPage_NoWarningsIsOneLine(t *testing.T) {
	var quiet []fleet.EventGroup
	for i := 0; i < 12; i++ {
		quiet = append(quiet, fleet.EventGroup{
			Event: model.Event{Type: "Normal", Reason: "Pulled", ObjectKind: "Pod",
				ObjectName: "p-" + strconv.Itoa(i), Message: "pulled " + strconv.Itoa(i),
				LastTimestamp: time.Now()},
			Occurrences: 1, Objects: 1,
		})
	}
	d := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Events:      fleet.Split[fleet.EventGroup]{Quiet: quiet},
		EventsTotal: 12,
	}

	body := get(t, serveDetail(t, d), "/cluster/capi/tenant-01").Body.String()

	if !strings.Contains(body, "No warnings &middot; 12 Normal event groups") {
		t.Errorf("the disclosure does not carry the good news:\n%s", body)
	}
	// The sentence this replaced.
	if strings.Contains(body, "Every event is Normal") {
		t.Error("the redundant sentence is back above the disclosure")
	}
	if strings.Contains(body, "No events reported") {
		t.Error("twelve folded groups reported as no events at all")
	}
}

// Each pane is in the column it was assigned, in the order it was assigned.
//
// Which pane goes where is a decision — the two whose height is decided by
// whatever is happening, unhealthy pods and events, go last in their column so
// that growing costs nothing. A pane that drifts out of position is a
// regression even though the page still renders.
func TestClusterPage_ColumnsHoldTheirPanes(t *testing.T) {
	body := get(t, serveDetail(t, richDetail()), "/cluster/capi/tenant-01").Body.String()

	cols := strings.Split(body, `<div class="col">`)
	if len(cols) != 3 {
		t.Fatalf("got %d column containers, want 2", len(cols)-1)
	}
	first, second := cols[1], cols[2]

	for _, want := range [][]string{
		{"<h3>Nodes", "<h3>Subsystems</h3>", "<h3>Network</h3>", "<h3>Cloud</h3>", "<h3>Unhealthy pods"},
		{"<h3>Node pools</h3>", "Machines &amp; hosts", "<h3>Events"},
	} {
		col, name := first, "first"
		if want[0] == "<h3>Node pools</h3>" {
			col, name = second, "second"
		}
		at := -1
		for _, h := range want {
			i := strings.Index(col, h)
			if i < 0 {
				t.Errorf("%s column is missing %s", name, h)
				continue
			}
			if i < at {
				t.Errorf("%s column has %s out of order", name, h)
			}
			at = i
		}
	}

	// And the variable-height panes really are last in their column.
	if i := strings.Index(first, "<h3>Unhealthy pods"); i < 0 || strings.Contains(first[i:], "<h3>Network") {
		t.Error("unhealthy pods is not last in the first column")
	}
	if i := strings.Index(second, "<h3>Events"); i < 0 || strings.Contains(second[i:], "Machines &amp; hosts") {
		t.Error("events is not last in the second column")
	}
}

// A pane in a column must not carry a span class: a column has one width, and a
// leftover span is the kind of dead attribute that later reads as intent.
func TestClusterPage_ColumnPanesHaveNoSpan(t *testing.T) {
	body := get(t, serveDetail(t, richDetail()), "/cluster/capi/tenant-01").Body.String()
	cols := body[strings.Index(body, `<div class="columns">`):]

	for _, span := range []string{"pane half", "pane wide", "pane narrow", "paired"} {
		if strings.Contains(cols, span) {
			t.Errorf("a pane in a column still declares %q", span)
		}
	}
}

// Network and Cloud sit in a column now, so pairing them is moot — but each
// still has to appear only when it has a source, which is the half of that rule
// that was about the data rather than the layout.
func TestClusterPage_NetworkAndCloudAppearWithTheirSources(t *testing.T) {
	both := fleet.ClusterDetail{
		ClusterView: fleet.ClusterView{Namespace: "capi", Name: "tenant-01", NodesKnown: true},
		Subsystems: fleet.Subsystems{
			Cilium:    &cilium.State{AgentsReady: 3, AgentsDesired: 3},
			Inventory: &openstack.Inventory{Counts: []openstack.Count{{Label: "Servers", Total: 1}}},
		},
	}
	body := get(t, serveDetail(t, both), "/cluster/capi/tenant-01").Body.String()
	if !strings.Contains(body, "<h3>Network</h3>") || !strings.Contains(body, "<h3>Cloud</h3>") {
		t.Error("a cluster reporting both subsystems is missing a pane")
	}

	networkOnly := both
	networkOnly.Subsystems.Inventory = nil
	body = get(t, serveDetail(t, networkOnly), "/cluster/capi/tenant-01").Body.String()
	if !strings.Contains(body, "<h3>Network</h3>") {
		t.Error("the network pane vanished with the cloud one")
	}
	if strings.Contains(body, "<h3>Cloud</h3>") {
		t.Error("a cloud pane rendered for a cluster with no cloud")
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

	// Node pools, machines, hosts, nodes, unhealthy pods. Each declares its own
	// column count, because a grid cannot infer one from the markup the way a
	// table does — and a wrong count silently misplaces every cell.
	if n := strings.Count(body, `<table class="packed cols-`); n != 5 {
		t.Errorf("got %d packed tables, want 5", n)
	}
	for _, want := range []string{
		`class="packed cols-4 spread"`, // node pools
		`class="packed cols-5 spread"`, // machines and hosts
		`class="packed cols-6 spread"`, // unhealthy pods
		`class="packed cols-7 spread"`, // nodes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no table declared %s", want)
		}
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
