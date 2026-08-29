package panes

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// TestMain forces a color-capable profile so styled output is real.
//
// Under `go test` there is no TTY, so lipgloss degrades to Ascii and strips
// styling; without this, any assertion comparing styled against plain output
// would pass vacuously.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func testRoles() profile.NodeRoles {
	return profile.NodeRoles{
		LabelKeys: []string{profile.UpstreamRoleLabelPrefix + profile.WildcardSuffix},
		Display: map[string]string{
			"control-plane": "Control-Plane",
			"compute":       "Compute",
		},
		MachineDeploymentMatch: map[string][]string{"compute": {"compute", "workers"}},
	}
}

func putSnap[T any](s *store.Store, key string, items ...T) {
	s.Put(key, model.Snapshot[T]{Items: items, UpdatedAt: time.Now()})
}

// plain strips styling so assertions can match on text.
func plain(s string) string {
	return termenv.String(s).String()
}

func contains(t *testing.T, body, want, ctx string) {
	t.Helper()
	if !strings.Contains(stripANSI(body), want) {
		t.Errorf("%s: output missing %q\n---\n%s\n---", ctx, want, body)
	}
}

func notContains(t *testing.T, body, unwanted, ctx string) {
	t.Helper()
	if strings.Contains(stripANSI(body), unwanted) {
		t.Errorf("%s: output should not contain %q\n---\n%s\n---", ctx, unwanted, body)
	}
}

// stripANSI removes escape sequences so tests assert on text, not styling.
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

// Every pane must respect the bounds it is given. A pane that overruns pushes
// the grid off the bottom of the terminal, so this is checked for all of them
// across a range of sizes, including absurdly small ones.
func TestAllPanes_RespectBounds(t *testing.T) {
	s := populatedStore()
	panes := []tui.Pane{
		NewOverview(s, testRoles(), ""),
		NewMachines(s, testRoles()),
		NewNodes(s, testRoles(), "v1.32.0"),
		NewPodHealth(s, []profile.CriticalWorkload{
			{Kind: "StatefulSet", Namespace: "kube-system", Name: "etcd"},
		}),
		NewEvents(s, ""),
	}

	for _, p := range panes {
		for _, w := range []int{20, 40, 80, 160, 300} {
			for _, h := range []int{1, 2, 5, 10, 40} {
				body := p.Render(w, h, false)
				if got := lipgloss.Height(body); body != "" && got > h {
					t.Errorf("%s at %dx%d: %d lines exceeds height", p.ID(), w, h, got)
				}
				for i, line := range strings.Split(body, "\n") {
					if got := lipgloss.Width(line); got > w {
						t.Errorf("%s at %dx%d: line %d width %d exceeds width",
							p.ID(), w, h, i, got)
					}
				}
			}
		}
	}
}

// A pane with no data must say it is loading, not render an empty frame that
// reads as "nothing is wrong".
func TestAllPanes_EmptyStoreShowsLoading(t *testing.T) {
	s := store.New()
	panes := []tui.Pane{
		NewMachines(s, testRoles()),
		NewNodes(s, testRoles(), ""),
		NewPodHealth(s, nil),
		NewEvents(s, ""),
	}
	for _, p := range panes {
		body := stripANSI(p.Render(60, 10, false))
		if !strings.Contains(body, "loading") {
			t.Errorf("%s with an empty store should say loading, got:\n%s", p.ID(), body)
		}
	}
}

// An error snapshot must surface, including the type-erased Snapshot[any] form a
// source publishes when it failed before it could build the real element type.
func TestAllPanes_SurfaceErrors(t *testing.T) {
	for _, key := range []string{model.KeyMgmtMachines, model.KeyWorkloadNodes, model.KeyWorkloadPods} {
		s := store.New()
		s.Put(key, model.ErrorSnapshot(errors.New("forbidden: cannot list")))

		var p tui.Pane
		switch key {
		case model.KeyMgmtMachines:
			p = NewMachines(s, testRoles())
		case model.KeyWorkloadNodes:
			p = NewNodes(s, testRoles(), "")
		default:
			p = NewPodHealth(s, nil)
		}
		body := stripANSI(p.Render(70, 8, false))
		if !strings.Contains(body, "forbidden") {
			t.Errorf("%s should surface the error, got:\n%s", p.ID(), body)
		}
	}
}

// populatedStore builds a small but realistic cluster: a control plane mid-roll,
// a worker pool, four hosts of which one is unclaimed, and a crash-looping pod.
func populatedStore() *store.Store {
	s := store.New()

	putSnap(s, model.KeyMgmtClusters, model.Cluster{
		Namespace: "capi", Name: "prod", Phase: "Provisioned", Available: true,
		Version:      "v1.32.0",
		ControlPlane: model.ReplicaBucket{Desired: 3, UpToDate: 1, Ready: 3},
		Workers:      model.ReplicaBucket{Desired: 2, UpToDate: 2, Ready: 2},
	})
	putSnap(s, model.KeyMgmtKCPs, model.KubeadmControlPlane{
		Namespace: "capi", Name: "prod-cp", Version: "v1.32.0",
		DesiredReplicas: 3, UpToDateReplicas: 1, ReadyReplicas: 3, Available: true,
	})
	putSnap(s, model.KeyMgmtMachineDeployments, model.MachineDeployment{
		Namespace: "capi", Name: "prod-compute", Version: "v1.32.0",
		DesiredReplicas: 2, UpToDateReplicas: 2, ReadyReplicas: 2, Available: true,
	})
	putSnap(s, model.KeyMgmtMachines,
		model.Machine{
			Namespace: "capi", Name: "prod-cp-1", Phase: "Running", Version: "v1.32.0",
			OwnerKind: "KubeadmControlPlane", OwnerName: "prod-cp",
			InfraKind: "Metal3Machine", InfraName: "cp-1-m3m", NodeName: "node-cp-1",
			Age: 72 * time.Hour,
		},
		model.Machine{
			Namespace: "capi", Name: "prod-compute-abc", Phase: "Provisioning", Version: "v1.32.0",
			OwnerKind: "MachineSet", OwnerName: "prod-compute-xyz",
			InfraKind: "Metal3Machine", InfraName: "compute-1-m3m",
			Age: 5 * time.Minute,
		},
	)
	putSnap(s, model.KeyMgmtMetal3Machines,
		model.Metal3Machine{Namespace: "capi", Name: "cp-1-m3m", Ready: true, BMHNamespace: "bmh", BMHName: "host-1"},
		model.Metal3Machine{Namespace: "capi", Name: "compute-1-m3m"},
	)
	putSnap(s, model.KeyMgmtBareMetalHosts,
		model.BareMetalHost{Namespace: "bmh", Name: "host-1", State: "provisioned", PoweredOn: true, Online: true},
		model.BareMetalHost{Namespace: "bmh", Name: "host-2", State: "available", Online: true},
		model.BareMetalHost{Namespace: "bmh", Name: "host-3", State: "inspecting"},
		model.BareMetalHost{Namespace: "bmh", Name: "host-4", State: "registering",
			ErrorMessage: "BMC unreachable"},
	)
	putSnap(s, model.KeyWorkloadNodes,
		model.Node{
			Name: "node-cp-1", Status: "Ready", Role: "control-plane", Version: "v1.32.0",
			AllocatableCPU: 4000, RequestedCPU: 1200,
			AllocatableMemory: 8 << 30, RequestedMemory: 3 << 30,
			Age: 72 * time.Hour,
		},
		model.Node{
			Name: "node-compute-1", Status: "Ready", Role: "compute", Version: "v1.31.0",
			Cordoned: true, AllocatableCPU: 8000, RequestedCPU: 7600,
			AllocatableMemory: 16 << 30, RequestedMemory: 15 << 30,
			Age: 71 * time.Hour, MemoryPressure: true,
		},
	)
	putSnap(s, model.KeyWorkloadPods,
		model.Pod{
			Namespace: "kube-system", Name: "etcd-0", ReadyReady: 1, ReadyTotal: 1,
			Status: "Running", IsHealthy: true, Node: "node-cp-1", Age: 72 * time.Hour,
		},
		model.Pod{
			Namespace: "kube-system", Name: "broken-abc", ReadyReady: 0, ReadyTotal: 1,
			Status: "CrashLoopBackOff", Restarts: 42, Node: "node-compute-1", Age: time.Hour,
		},
	)
	putSnap(s, model.KeyWorkloadWorkloads,
		model.Workload{Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Ready: 2, Desired: 2},
		model.Workload{Kind: "DaemonSet", Namespace: "kube-system", Name: "kube-proxy", Ready: 1, Desired: 2},
	)
	putSnap(s, model.KeyWorkloadEvents,
		model.Event{
			Namespace: "kube-system", Type: "Warning", Reason: "BackOff", Count: 30,
			ObjectKind: "Pod", ObjectName: "broken-abc", Message: "back-off restarting",
			LastTimestamp: time.Now().Add(-2 * time.Minute),
		},
		model.Event{
			Namespace: "kube-system", Type: "Normal", Reason: "Pulled", Count: 100,
			ObjectKind: "Pod", ObjectName: "coredns-1", Message: "image pulled",
			LastTimestamp: time.Now().Add(-time.Hour),
		},
	)
	return s
}

// --- overview -------------------------------------------------------------

// During a rollout the pane reports progress; at rest it reports readiness.
// Showing progress bars at rest would be a row of full green bars saying nothing.
func TestOverview_ModeAware(t *testing.T) {
	s := populatedStore()

	rolling := NewOverview(s, testRoles(), "").Render(90, 30, false)
	contains(t, rolling, "Rollout Progress", "mid-rollout")
	contains(t, rolling, "1/3 updated", "mid-rollout")
	notContains(t, rolling, "Node Pools", "mid-rollout")

	// Bring the control plane up to date; the mode should flip.
	putSnap(s, model.KeyMgmtKCPs, model.KubeadmControlPlane{
		Namespace: "capi", Name: "prod-cp", Version: "v1.32.0",
		DesiredReplicas: 3, UpToDateReplicas: 3, ReadyReplicas: 3, Available: true,
	})
	settled := NewOverview(s, testRoles(), "").Render(90, 30, false)
	contains(t, settled, "Node Pools", "steady state")
	contains(t, settled, "3/3 ready", "steady state")
	notContains(t, settled, "updated", "steady state")
}

// A stated target version activates the mode before anything has rolled.
func TestOverview_TargetVersionAssertsRollout(t *testing.T) {
	s := store.New()
	putSnap(s, model.KeyMgmtKCPs, model.KubeadmControlPlane{
		Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 3,
	})
	putSnap(s, model.KeyMgmtClusters, model.Cluster{Namespace: "capi", Name: "prod", Phase: "Provisioned"})

	body := NewOverview(s, testRoles(), "v1.33.0").Render(90, 20, false)
	contains(t, body, "Rollout Progress", "asserted rollout")
	contains(t, body, "target v1.33.0", "asserted rollout")
}

func TestOverview_RolesAndPressure(t *testing.T) {
	body := NewOverview(populatedStore(), testRoles(), "").Render(90, 30, false)
	// Display names from the profile, not raw role strings.
	contains(t, body, "Control-Plane", "role display")
	contains(t, body, "Compute", "role display")
	contains(t, body, "1 cordoned", "cordon count")
	contains(t, body, "memory×1", "pressure")
}

// A pressure row of permanent zeroes trains the eye to skip it, so absent
// pressures are not named at all.
func TestOverview_NoPressureRowWhenHealthy(t *testing.T) {
	s := store.New()
	putSnap(s, model.KeyWorkloadNodes, model.Node{Name: "n1", Status: "Ready", Role: "compute"})
	body := NewOverview(s, testRoles(), "").Render(80, 20, false)
	notContains(t, body, "pressure", "healthy nodes")
}

// With no Cluster API at all the pane still carries node and workload data
// rather than collapsing — that is the plain-cluster case.
func TestOverview_WorksWithoutClusterAPI(t *testing.T) {
	s := store.New()
	putSnap(s, model.KeyWorkloadNodes, model.Node{Name: "n1", Status: "Ready", Role: "compute", Version: "v1.32.0"})
	putSnap(s, model.KeyWorkloadWorkloads,
		model.Workload{Kind: "Deployment", Namespace: "default", Name: "app", Ready: 1, Desired: 1})

	body := NewOverview(s, testRoles(), "").Render(80, 20, false)
	contains(t, body, "Nodes", "no Cluster API")
	contains(t, body, "Workloads", "no Cluster API")
	notContains(t, body, "loading", "no Cluster API")
}

func TestOverview_FixedHeightMatchesContent(t *testing.T) {
	p := NewOverview(populatedStore(), testRoles(), "")
	want := lipgloss.Height(p.Render(80, 100, false))
	if got := p.FixedHeight(80); got < want {
		t.Errorf("FixedHeight %d is less than the content's %d lines", got, want)
	}
}

func TestOverview_FixedWidth(t *testing.T) {
	p := NewOverview(store.New(), testRoles(), "")
	if got := p.FixedWidth(300); got != 0 {
		t.Errorf("unset fraction should mean full width, got %d", got)
	}
	if got := p.WithWidthFraction(3).FixedWidth(300); got != 100 {
		t.Errorf("got %d want 100", got)
	}
}

// --- machines -------------------------------------------------------------

// The join is the pane's reason to exist, so the full chain must appear.
func TestMachines_JoinsMachineToHost(t *testing.T) {
	body := NewMachines(populatedStore(), testRoles()).Render(120, 20, false)
	contains(t, body, "prod-cp-1", "machine name")
	contains(t, body, "host-1", "joined host")
	contains(t, body, "provisioned", "host state")
	contains(t, body, "Control-Plane", "role")
}

// A machine whose provider machine exists but has no host reads differently from
// one with no provider machine at all: it usually means no host matched.
func TestMachines_AwaitingHost(t *testing.T) {
	body := NewMachines(populatedStore(), testRoles()).Render(120, 20, false)
	contains(t, body, "awaiting host", "unbound provider machine")
}

func TestMachines_UnclaimedHostSummary(t *testing.T) {
	body := NewMachines(populatedStore(), testRoles()).Render(120, 20, false)
	contains(t, body, "Unclaimed hosts (3)", "unclaimed count")
	contains(t, body, "available", "unclaimed states")
	contains(t, body, "1 with errors", "errored hosts")
}

// A machine row must never be hidden behind the unclaimed summary.
func TestMachines_SummaryYieldsToRows(t *testing.T) {
	body := NewMachines(populatedStore(), testRoles()).Render(120, 4, false)
	contains(t, body, "prod-cp-1", "tight height")
	notContains(t, body, "Unclaimed", "tight height")
}

// Cluster API on another provider still lists its machines, with empty host
// columns rather than an error.
func TestMachines_NonMetal3Provider(t *testing.T) {
	s := store.New()
	putSnap(s, model.KeyMgmtMachines, model.Machine{
		Namespace: "capi", Name: "aws-1", Phase: "Running",
		InfraKind: "AWSMachine", InfraName: "aws-machine-1",
	})
	body := NewMachines(s, testRoles()).Render(100, 10, false)
	contains(t, body, "aws-1", "non-Metal3 machine listed")
	contains(t, body, "unbound", "no host")
}

func TestMachines_RoleFromMachineSetName(t *testing.T) {
	body := NewMachines(populatedStore(), testRoles()).Render(120, 20, false)
	// prod-compute-abc is owned by MachineSet prod-compute-xyz, which carries
	// the pool name.
	contains(t, body, "Compute", "role from MachineSet name")
}

// --- nodes ----------------------------------------------------------------

func TestNodes_Table(t *testing.T) {
	body := NewNodes(populatedStore(), testRoles(), "v1.32.0").Render(110, 20, false)
	contains(t, body, "node-cp-1", "node name")
	contains(t, body, "Cordoned", "cordon state")
	contains(t, body, "Control-Plane", "role display")
	contains(t, body, "Cluster:", "total line")
}

// A mixed-version node list is the rollout seen from the workload side, so the
// version column has to be present and comparable.
func TestNodes_ShowsMixedVersions(t *testing.T) {
	body := NewNodes(populatedStore(), testRoles(), "v1.32.0").Render(110, 20, false)
	contains(t, body, "v1.32.0", "target version node")
	contains(t, body, "v1.31.0", "lagging node")
}

func TestNodes_ResourcePercentages(t *testing.T) {
	body := NewNodes(populatedStore(), testRoles(), "").Render(110, 20, false)
	// node-cp-1 requests 1200 of 4000 millicores.
	contains(t, body, "30%", "cpu percentage")
	// node-compute-1 requests 7600 of 8000.
	contains(t, body, "95%", "high cpu percentage")
}

func TestNodes_TotalLineYieldsToRows(t *testing.T) {
	body := NewNodes(populatedStore(), testRoles(), "").Render(110, 4, false)
	notContains(t, body, "Cluster:", "tight height")
}

// --- pods -----------------------------------------------------------------

// A pinned workload with no pods is the case this block exists for; an
// unhealthy-only list cannot report a StatefulSet that is simply gone.
func TestPods_CriticalWorkloadMissing(t *testing.T) {
	s := populatedStore()
	p := NewPodHealth(s, []profile.CriticalWorkload{
		{Kind: "StatefulSet", Namespace: "openstack", Name: "database"},
	})
	body := p.Render(100, 20, false)
	contains(t, body, "openstack/database", "pinned workload")
	contains(t, body, "missing", "missing state")
}

func TestPods_CriticalWorkloadHealthy(t *testing.T) {
	p := NewPodHealth(populatedStore(), []profile.CriticalWorkload{
		{Kind: "StatefulSet", Namespace: "kube-system", Name: "etcd"},
	})
	body := p.Render(100, 20, false)
	contains(t, body, "kube-system/etcd", "pinned workload")
	contains(t, body, "healthy", "healthy state")
}

func TestPods_UnhealthyList(t *testing.T) {
	body := NewPodHealth(populatedStore(), nil).Render(100, 20, false)
	contains(t, body, "Unhealthy pods (1)", "count")
	contains(t, body, "broken-abc", "pod name")
	contains(t, body, "CrashLoopBackOff", "status")
	contains(t, body, "42", "restart count")
}

func TestPods_AllHealthy(t *testing.T) {
	s := store.New()
	putSnap(s, model.KeyWorkloadPods, model.Pod{
		Namespace: "default", Name: "ok", ReadyReady: 1, ReadyTotal: 1,
		Status: "Running", IsHealthy: true,
	})
	contains(t, NewPodHealth(s, nil).Render(80, 10, false), "All pods healthy", "healthy cluster")
}

// When there is no room for the table the count must still be visible: silently
// dropping it would read as "nothing is wrong".
func TestPods_TightHeightStillReportsCount(t *testing.T) {
	body := NewPodHealth(populatedStore(), []profile.CriticalWorkload{
		{Kind: "StatefulSet", Namespace: "kube-system", Name: "etcd"},
	}).Render(100, 5, false)
	if !strings.Contains(stripANSI(body), "unhealthy") {
		t.Errorf("expected the unhealthy count to survive a tight pane:\n%s", body)
	}
}

// --- events ---------------------------------------------------------------

// settledStore is populatedStore with the rollout finished, so the events pane
// reads the workload cluster. The distinction is the pane's whole mode-awareness,
// so the tests have to be explicit about which state they are in.
func settledStore() *store.Store {
	s := populatedStore()
	putSnap(s, model.KeyMgmtKCPs, model.KubeadmControlPlane{
		Namespace: "capi", Name: "prod-cp", Version: "v1.32.0",
		DesiredReplicas: 3, UpToDateReplicas: 3, ReadyReplicas: 3, Available: true,
	})
	return s
}

// Rollups are the default because a chronological list is one repeated line on a
// busy cluster.
func TestEvents_RollupByReason(t *testing.T) {
	body := NewEvents(settledStore(), "").Render(110, 20, false)
	contains(t, body, "BackOff", "reason")
	contains(t, body, "Pulled", "reason")
	// Counts come from each event's own Count, since Kubernetes deduplicates
	// server-side and one object can stand for hundreds of occurrences.
	contains(t, body, "30", "warning count")
	contains(t, body, "100", "normal count")
}

// A single Warning matters more than a hundred Normal events, so sorting by
// count alone would bury it.
func TestEvents_WarningsFirst(t *testing.T) {
	body := stripANSI(NewEvents(settledStore(), "").Render(110, 20, false))
	warnIdx := strings.Index(body, "BackOff")
	normalIdx := strings.Index(body, "Pulled")
	if warnIdx < 0 || normalIdx < 0 {
		t.Fatalf("both reasons should appear:\n%s", body)
	}
	if warnIdx > normalIdx {
		t.Errorf("the Warning should sort above the higher-count Normal:\n%s", body)
	}
}

func TestEvents_Expanded(t *testing.T) {
	p := NewEvents(settledStore(), "")
	if p.Expanded() {
		t.Error("should start collapsed")
	}
	p.ToggleExpanded()
	if !p.Expanded() {
		t.Error("ToggleExpanded should switch mode")
	}
	body := p.Render(110, 20, false)
	contains(t, body, "back-off restarting", "expanded shows messages")
}

// The title says which cluster the events came from, so the reader is never in
// doubt about whose events these are.
func TestEvents_TitleNamesSource(t *testing.T) {
	s := settledStore()
	if got := NewEvents(s, "").Title(); !strings.Contains(got, "workload") {
		t.Errorf("steady state title: got %q want it to mention workload", got)
	}
	if got := NewEvents(s, "v1.33.0").Title(); !strings.Contains(got, "management") {
		t.Errorf("rollout title: got %q want it to mention management", got)
	}
}

// During a rollout the pane reads the management cluster's events.
func TestEvents_SwitchesSourceDuringRollout(t *testing.T) {
	s := populatedStore()
	putSnap(s, model.KeyMgmtEvents, model.Event{
		Namespace: "capi", Type: "Normal", Reason: "MachineCreated", Count: 1,
		ObjectKind: "Machine", ObjectName: "prod-cp-2", LastTimestamp: time.Now(),
	})
	body := NewEvents(s, "v1.33.0").Render(110, 20, false)
	contains(t, body, "MachineCreated", "management events during rollout")
	notContains(t, body, "BackOff", "workload events excluded during rollout")
}

// --- helpers --------------------------------------------------------------

func TestProgressBar(t *testing.T) {
	if got := lipgloss.Width(stripANSI(progressBar(10, 5, 10))); got != 10 {
		t.Errorf("bar width: got %d want 10", got)
	}
	// A zero total is unknown, not complete.
	if got := stripANSI(progressBar(6, 0, 0)); !strings.Contains(got, "·") {
		t.Errorf("zero total should render as unknown, got %q", got)
	}
	if got := progressBar(2, 1, 2); got != "" {
		t.Errorf("a bar too narrow to read should be omitted, got %q", got)
	}
}

func TestPct(t *testing.T) {
	if got := pct(50, 200); got != "25%" {
		t.Errorf("got %q want 25%%", got)
	}
	// A zero denominator must not divide by zero or claim 0%.
	if got := pct(0, 0); got != "—" {
		t.Errorf("zero denominator: got %q want an em dash", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		512:     "512B",
		2048:    "2.0K",
		5 << 20: "5.0M",
		3 << 30: "3.0G",
		2 << 40: "2.0T",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d): got %q want %q", in, got, want)
		}
	}
}

func TestMillicores(t *testing.T) {
	if got := millicores(1500); got != "1500m" {
		t.Errorf("got %q want 1500m", got)
	}
	// Past ten cores, millicores stop being readable.
	if got := millicores(64000); got != "64.0" {
		t.Errorf("got %q want 64.0", got)
	}
}

func TestPctStyle(t *testing.T) {
	tests := []struct {
		part, whole int64
		want        lipgloss.Style
	}{
		{50, 100, tui.StyleOK},
		{90, 100, tui.StyleWarn},
		{100, 100, tui.StyleErr},
		{120, 100, tui.StyleErr},
		{0, 0, tui.StyleMuted},
	}
	for _, tc := range tests {
		if got := pctStyle(tc.part, tc.whole); got.GetForeground() != tc.want.GetForeground() {
			t.Errorf("pctStyle(%d,%d): got %v want %v",
				tc.part, tc.whole, got.GetForeground(), tc.want.GetForeground())
		}
	}
}

func TestCountStyle(t *testing.T) {
	if got := countStyle(3, 3); got.GetForeground() != tui.StyleOK.GetForeground() {
		t.Error("complete should be OK")
	}
	if got := countStyle(1, 3); got.GetForeground() != tui.StyleWarn.GetForeground() {
		t.Error("partial should warn")
	}
	if got := countStyle(0, 3); got.GetForeground() != tui.StyleMuted.GetForeground() {
		t.Error("nothing started should be muted")
	}
	if got := countStyle(0, 0); got.GetForeground() != tui.StyleMuted.GetForeground() {
		t.Error("nothing to do should be muted")
	}
}

func TestPlainHelperIsUnused(t *testing.T) {
	// Keeps the helper honest if it is ever reached for.
	if plain("x") != "x" {
		t.Error("plain should pass through unstyled text")
	}
}

// --- cordoned by design ---------------------------------------------------

// A site whose hypervisors are permanently cordoned must not be painted amber
// for the life of the cluster: the color has to keep meaning something.
func reservedComputeRoles() profile.NodeRoles {
	r := testRoles()
	r.CordonExpected = []string{"compute"}
	return r
}

func cordonedFleet() *store.Store {
	s := store.New()
	putSnap(s, model.KeyWorkloadNodes,
		model.Node{
			Name: "node-cp-1", Status: "Ready", Role: "control-plane", Version: "v1.32.0",
			AllocatableCPU: 4000, RequestedCPU: 1000,
			AllocatableMemory: 8 << 30, RequestedMemory: 2 << 30,
		},
		model.Node{
			Name: "node-compute-1", Status: "Ready", Role: "compute", Version: "v1.32.0",
			Cordoned: true, AllocatableCPU: 8000, RequestedCPU: 1000,
			AllocatableMemory: 16 << 30, RequestedMemory: 2 << 30,
		},
	)
	return s
}

func TestNodes_ExpectedCordonIsNotCounted(t *testing.T) {
	s := cordonedFleet()

	body := stripANSI(NewNodes(s, reservedComputeRoles(), "").Render(110, 20, false))
	notContains(t, body, "cordoned", "expected cordon in the total line")
	// The row still says so, because it is true and worth knowing.
	contains(t, body, "Cordoned", "cordon still shown per node")

	// Without the profile setting, the same fleet still reports it.
	plainRoles := stripANSI(NewNodes(s, testRoles(), "").Render(110, 20, false))
	contains(t, plainRoles, "1 cordoned", "unconfigured profile still counts cordons")
}

func TestNodes_ExpectedCordonIsNotAmber(t *testing.T) {
	p := NewNodes(cordonedFleet(), reservedComputeRoles(), "")
	node := model.Node{Name: "n", Status: "Ready", Role: "compute", Cordoned: true}

	if got := p.statusStyle(node); got.GetForeground() != tui.StyleOK.GetForeground() {
		t.Error("an expected cordon should read as healthy, not as a warning")
	}
	// A role that is not exempt keeps the warning.
	if got := NewNodes(cordonedFleet(), testRoles(), "").statusStyle(node); got.GetForeground() != tui.StyleWarn.GetForeground() {
		t.Error("an unexpected cordon should still warn")
	}
}

// A node that is cordoned *and* down must not read as "no opinion". Styling the
// composed "NotReady,Cordoned" token falls through to muted, which is how a
// broken node ends up looking unremarkable.
func TestNodes_CordonedAndDownReadsAsAFailure(t *testing.T) {
	node := model.Node{Name: "n", Status: "NotReady", Role: "compute", Cordoned: true}
	for _, roles := range []profile.NodeRoles{testRoles(), reservedComputeRoles()} {
		p := NewNodes(cordonedFleet(), roles, "")
		if got := p.statusStyle(node); got.GetForeground() != tui.StyleErr.GetForeground() {
			t.Errorf("cordoned and NotReady should read as an error, got %v", got.GetForeground())
		}
	}
}

func TestOverview_ExpectedCordonAddsNoWarning(t *testing.T) {
	s := cordonedFleet()

	body := stripANSI(NewOverview(s, reservedComputeRoles(), "").Render(80, 20, false))
	notContains(t, body, "cordoned", "expected cordon in the role summary")
	contains(t, body, "1/1 ready", "compute still counted as ready")

	plainRoles := stripANSI(NewOverview(s, testRoles(), "").Render(80, 20, false))
	contains(t, plainRoles, "1 cordoned", "unconfigured profile still reports it")
}

// A snapshot with a note is not ready rather than empty, and the pane says what it
// is waiting for. "Loading" for minutes with no reason is indistinguishable from a
// pane that is broken.
func TestPanes_NoteRendersInsteadOfAnEmptyTable(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{
		Note: "listing events in every namespace — this can take minutes",
	})

	body := stripANSI(NewEvents(s, "").Render(90, 8, false))
	contains(t, body, "listing events in every namespace", "note as a placeholder")
	notContains(t, body, "No events", "a note is not an empty result")
}

// Once there is data, the note is gone and the data wins.
func TestPanes_NoteYieldsToData(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadEvents, model.Snapshot[model.Event]{
		Items: []model.Event{{
			Type: "Warning", Reason: "Unhealthy", Namespace: "kube-system",
			ObjectName: "pod-1", Message: "probe failed", Count: 3,
			LastTimestamp: time.Now(),
		}},
		Note:      "should not be shown",
		UpdatedAt: time.Now(),
	})

	body := stripANSI(NewEvents(s, "").Render(90, 8, false))
	contains(t, body, "Unhealthy", "the event")
	notContains(t, body, "should not be shown", "note suppressed once data exists")
}

// --- contributed summary blocks -------------------------------------------

// A contributed block must never make the overview taller. It is a fixed-height
// row above the whole grid, so growing it pushes every other pane down — and it
// would do so exactly when a subsystem started misbehaving, which is the worst
// possible moment for the layout to move.
func TestOverview_ContributedBlockNeverAddsHeight(t *testing.T) {
	s := populatedStore()
	const width = 276

	plain := NewOverview(s, testRoles(), "")
	baseHeight := plain.FixedHeight(width)

	withBlock := NewOverview(s, testRoles(), "").
		WithSummaries(func() []tui.SummaryBlock {
			return []tui.SummaryBlock{{
				Title: "Ceph",
				// Deliberately more lines than any core block, to prove the pane
				// trims rather than trusting the plugin.
				Lines: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
			}}
		})
	if got := withBlock.FixedHeight(width); got != baseHeight {
		t.Errorf("height %d with a contributed block, %d without: a block must add a "+
			"column, never a row", got, baseHeight)
	}
	if !strings.Contains(stripANSI(withBlock.Render(width, baseHeight, false)), "Ceph") {
		t.Error("the contributed block should be visible when there is a column for it")
	}
}

// And when there is no column to spare it does not appear at all, rather than
// stacking under an existing block and lengthening the row.
func TestOverview_ContributedBlockDroppedWhenNoColumnIsFree(t *testing.T) {
	s := populatedStore()
	supplier := func() []tui.SummaryBlock {
		return []tui.SummaryBlock{{Title: "Ceph", Lines: []string{"health HEALTH_WARN"}}}
	}

	narrow := NewOverview(s, testRoles(), "").WithSummaries(supplier)
	const width = 90 // room for one column of blocks; the core blocks already stack

	plain := NewOverview(s, testRoles(), "")
	if got, want := narrow.FixedHeight(width), plain.FixedHeight(width); got != want {
		t.Errorf("height %d with a block at %d columns, %d without", got, width, want)
	}
	if strings.Contains(stripANSI(narrow.Render(width, 20, false)), "Ceph") {
		t.Error("a block with no column of its own should be dropped, not stacked")
	}
}

// No supplier at all must behave exactly as before the slot existed.
func TestOverview_NoSupplierIsUnchanged(t *testing.T) {
	s := populatedStore()
	const width = 276
	a := NewOverview(s, testRoles(), "").Render(width, 12, false)
	b := NewOverview(s, testRoles(), "").WithSummaries(nil).Render(width, 12, false)
	if a != b {
		t.Error("a nil supplier should render identically to no supplier")
	}
}

// --- content extents ------------------------------------------------------

// The complaint this answers: a fleet with long machine names, rendered in a
// quarter of a wide terminal, truncates every MACHINE cell to a shared prefix like
// "demo-workers-" — rows a reader cannot tell apart. The pane cannot fix that
// alone; what it can do is say how much width would fix it, which is what the
// layout divides its rows by.
func TestMachines_ContentWidthShowsEveryName(t *testing.T) {
	s := store.New()
	long := "demo-workers-5d9c7-lm2vp"
	putSnap(s, model.KeyMgmtMachines, model.Machine{
		Name: long, Phase: "Running", Version: "v1.36.2", Age: time.Hour,
	})
	p := NewMachines(s, testRoles())

	want := p.ContentWidth()
	if want <= 0 {
		t.Fatal("a pane with machines to show should declare a width")
	}
	// Rendered at the width it asked for, no cell is cut.
	contains(t, p.Render(want, 10, false), long, "machine name at the declared width")

	// And the number is honest about being about *this* fleet: a longer name asks
	// for more.
	s2 := store.New()
	putSnap(s2, model.KeyMgmtMachines, model.Machine{
		Name: long + "-with-a-longer-suffix", Phase: "Running", Age: time.Hour,
	})
	if got := NewMachines(s2, testRoles()).ContentWidth(); got <= want {
		t.Errorf("a longer machine name should ask for more width: got %d, was %d", got, want)
	}
}

// Nodes are identified by an FQDN, which is the cell a narrow tile takes away
// first.
func TestNodes_ContentWidthShowsEveryFQDN(t *testing.T) {
	s := store.New()
	fqdn := "compute-node-1.site-a.demo.example"
	putSnap(s, model.KeyWorkloadNodes, model.Node{
		Name: fqdn, Status: "Ready", Version: "v1.35.3", Age: time.Hour,
	})
	p := NewNodes(s, testRoles(), "")

	want := p.ContentWidth()
	if want <= 0 {
		t.Fatal("a pane with nodes to show should declare a width")
	}
	contains(t, p.Render(want, 10, false), fqdn, "node FQDN at the declared width")
}

// An empty or unread store declares nothing rather than guessing, which the
// layout reads as "no preference" and answers with an average share.
func TestContentWidth_UnknownWhenThereIsNoData(t *testing.T) {
	s := store.New()
	for _, p := range []tui.ContentWidthPane{
		NewMachines(s, testRoles()),
		NewNodes(s, testRoles(), ""),
		NewPodHealth(s, nil),
		NewEvents(s, ""),
	} {
		if got := p.ContentWidth(); got != 0 {
			t.Errorf("pane %q: got %d want 0 with an empty store", p.ID(), got)
		}
	}
}

// The regression that made the content-sized layout usable: a rolling fleet
// changes HOST STATE on every poll — "provisioned", "deprovisioning powered off",
// a Metal3 error — and none of it changes which machine a row is. Charging the
// pane for that text moved its declared width by 37 cells, which slid every tile
// boundary in its band sideways on a twenty-second timer.
func TestMachines_ContentWidthHoldsStillWhileHostsChange(t *testing.T) {
	settled := NewMachines(populatedStore(), testRoles()).ContentWidth()

	churning := populatedStore()
	putSnap(churning, model.KeyMgmtBareMetalHosts,
		model.BareMetalHost{
			Namespace: "bmh", Name: "host-1", State: "deprovisioning",
			ConsumerName: "cp-1-m3m", PoweredOn: false,
			ErrorMessage: "Introspection timed out after 30m0s",
		},
		model.BareMetalHost{Namespace: "bmh", Name: "host-2", State: "available", Online: true},
	)
	if got := NewMachines(churning, testRoles()).ContentWidth(); got != settled {
		t.Errorf("declared width moved with the host state: %d then %d", settled, got)
	}
}
