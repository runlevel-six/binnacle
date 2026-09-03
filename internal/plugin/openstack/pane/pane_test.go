package pane

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/internal/plugin/openstack"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
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

func realAgents() []Agent {
	return []Agent{
		{Service: ServiceCompute, Binary: "nova-compute", Host: "compute-node-1.site-a.example.com",
			Zone: "nova", Up: true, Enabled: true},
		{Service: ServiceCompute, Binary: "nova-compute", Host: "compute-node-2.site-a.example.com",
			Zone: "nova", Up: true, Enabled: true},
		{Service: ServiceCompute, Binary: "nova-scheduler", Host: "nova-scheduler-5f46b85bdf-j58jq",
			Zone: "internal", Up: true, Enabled: true},
		{Service: ServiceCompute, Binary: "nova-conductor", Host: "nova-conductor-abc-1",
			Zone: "internal", Up: true, Enabled: true},
		{Service: ServiceNetwork, Binary: "neutron-ovn-metadata-agent", Host: "compute-node-1.site-a.example.com",
			Up: true, Enabled: true},
		{Service: ServiceBlockStorage, Binary: "cinder-scheduler", Host: "cinder-scheduler-xyz",
			Zone: "internal", Up: true, Enabled: true},
	}
}

func stateFrom(agents []Agent) State {
	return State{
		Cloud: "my-cloud", Region: "dev-1",
		Agents: agents, Services: openstack.Summarize(agents), UpdatedAt: time.Now(),
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func at(t *testing.T, s string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return got
}

func wl(ns, app, part, kind string, desired, updated int32, manual bool) model.Workload {
	name := app
	if part != "" {
		name = app + "-" + part
	}
	return model.Workload{
		Namespace: ns, Name: name, Kind: kind,
		Desired: desired, Updated: updated, Ready: desired, Manual: manual,
		Labels: map[string]string{"application": app, "component": part},
	}
}

func withWorkloads(items ...model.Workload) *store.Store {
	s := store.New()
	s.Put(model.KeyWorkloadWorkloads, model.Snapshot[model.Workload]{Items: items})
	return s
}

// --- main pane ---------------------------------------------------------------

func TestPane_ServiceTable(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(realAgents()))
	body := stripANSI(newPane(s).Render(90, 10, false))

	for _, want := range []string{"compute", "network", "block-storage", "4/4 up"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
}

func TestPane_DrainedAndBrokenAreDistinguished(t *testing.T) {
	agents := append(realAgents(),
		Agent{Service: ServiceCompute, Binary: "nova-compute",
			Host: "compute-node-3.site-a.example.com", Up: false, Enabled: false},
		Agent{Service: ServiceCompute, Binary: "nova-compute",
			Host: "compute-node-4.site-a.example.com", Up: false, Enabled: true},
	)
	s := store.New()
	s.Put(KeyState, stateFrom(agents))
	body := stripANSI(newPane(s).Render(100, 12, false))

	if !strings.Contains(body, "Down:") || !strings.Contains(body, "compute-node-4") {
		t.Errorf("the broken agent should be named under Down:\n%s", body)
	}
	if !strings.Contains(body, "Disabled:") || !strings.Contains(body, "compute-node-3") {
		t.Errorf("the drained agent should be named under Disabled:\n%s", body)
	}
	if strings.Contains(body, "site-a.example.com") {
		t.Errorf("host domains should be trimmed:\n%s", body)
	}
}

func TestPane_PerServiceErrorIsIsolated(t *testing.T) {
	st := stateFrom(realAgents())
	st.Services = append(st.Services, ServiceSummary{Service: "shared-file-system", Err: errFake{}})

	s := store.New()
	s.Put(KeyState, st)
	body := stripANSI(newPane(s).Render(100, 12, false))

	if !strings.Contains(body, "4/4 up") {
		t.Errorf("the healthy services should still render:\n%s", body)
	}
	if !strings.Contains(body, "unavailable") {
		t.Errorf("the failed service should say so:\n%s", body)
	}
}

func TestPane_States(t *testing.T) {
	if got := stripANSI(newPane(store.New()).Render(60, 8, false)); !strings.Contains(got, "loading") {
		t.Errorf("got %q want loading", got)
	}
	s := store.New()
	s.Put(KeyState, State{Err: errFake{}, UpdatedAt: time.Now()})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "boom") {
		t.Errorf("got %q want the error", got)
	}
}

func TestPane_RespectsBounds(t *testing.T) {
	agents := append(realAgents(), Agent{Service: ServiceCompute, Binary: "nova-compute",
		Host: "compute-node-4.site-a.example.com", Up: false, Enabled: true})
	s := store.New()
	s.Put(KeyState, stateFrom(agents))
	p := newPane(s)

	for _, w := range []int{20, 46, 90, 220} {
		for _, h := range []int{1, 3, 9, 30} {
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

func TestShortHost(t *testing.T) {
	tests := map[string]string{
		"compute-node-1.site-a.example.com": "compute-node-1",
		"nova-scheduler-5f46b85bdf-j58jq":   "nova-scheduler-5f46b85bdf-j58jq",
		"":                                  "",
	}
	for in, want := range tests {
		if got := ShortHost(in); got != want {
			t.Errorf("ShortHost(%q): got %q want %q", in, got, want)
		}
	}
}

// --- services pane -----------------------------------------------------------

func TestServicesPaneShortAndConvergedReportsTheCount(t *testing.T) {
	items := make([]model.Workload, 0, 11)
	for _, app := range []string{
		"keystone", "glance", "nova", "neutron", "cinder", "heat",
		"octavia", "barbican", "placement", "magnum", "manila",
	} {
		items = append(items, wl("openstack", app, "api", "Deployment", 3, 3, false))
	}

	body := stripANSI(newServicesPane(withWorkloads(items...), "openstack").Render(60, 4, false))
	if !strings.Contains(body, "11 service(s) up to date") {
		t.Errorf("want the whole-cloud count in a short frame, got:\n%s", body)
	}
	if strings.Contains(body, "+ ") {
		t.Errorf("a converged cloud should not render a truncated table:\n%s", body)
	}
}

func TestServicesPaneKeepsPendingRowsOverTheSummary(t *testing.T) {
	s := withWorkloads(
		wl("openstack", "keystone", "api", "Deployment", 3, 3, false),
		wl("openstack", "glance", "api", "Deployment", 3, 3, false),
		wl("openstack", "nova", "compute", "DaemonSet", 5, 2, false),
		wl("openstack", "neutron", "ovn-metadata-agent", "DaemonSet", 5, 3, false),
		wl("openstack", "libvirt", "libvirt", "DaemonSet", 5, 1, true),
	)

	body := stripANSI(newServicesPane(s, "openstack").Render(60, 4, false))
	for _, want := range []string{"libvirt", "nova", "neutron"} {
		if !strings.Contains(body, want) {
			t.Errorf("pending service %q was displaced:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "⚠") {
		t.Errorf("the manual marker is missing:\n%s", body)
	}
}

// --- cloud pane --------------------------------------------------------------

func TestCloudPaneTitleIsStable(t *testing.T) {
	s := store.New()
	for _, target := range []string{"", "v1.31.4"} {
		if got := newCloudPane(s, target).Title(); got != "Server Migrations" {
			t.Errorf("target %q: title = %q, want Server Migrations", target, got)
		}
	}
}

func TestPluginContributesDistinctPanes(t *testing.T) {
	panes := NewProvider("openstack", "v1.31.4").Panes(store.New())
	if len(panes) != 4 {
		t.Fatalf("got %d panes, want 4", len(panes))
	}
	seen := map[string]bool{}
	for _, pane := range panes {
		if seen[pane.ID()] {
			t.Fatalf("duplicate pane ID %q", pane.ID())
		}
		seen[pane.ID()] = true
	}
	for _, i := range []int{0, 1, 2} {
		if _, ok := panes[i].(tui.GroupedPane); !ok {
			t.Errorf("pane %q is not grouped", panes[i].ID())
		}
	}
	if _, ok := panes[3].(tui.GroupedPane); ok {
		t.Errorf("migrations pane %q should not be grouped", panes[3].ID())
	}
}

func TestCloudPaneRendersMigrationsDuringRollout(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: []Migration{
		{ID: 1, InstanceUUID: "abcdef1234567890", Status: "running", Type: "live-migration",
			SourceCompute: "compute-node-1.site-a.example.com",
			DestCompute:   "compute-node-2.site-a.example.com",
			UpdatedAt:     now.Add(-2 * time.Minute)},
		{ID: 2, InstanceUUID: "fedcba0987654321", Status: "failed", Type: "live-migration",
			SourceCompute: "compute-node-3.site-a.example.com",
			UpdatedAt:     now.Add(-30 * time.Minute)},
	}})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 10, now))

	for _, want := range []string{
		"1 active", "1 failed",
		"abcdef12",
		"compute-node-1 → compute-node-2",
		"compute-node-3 → ?",
		"running", "failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "site-a.example.com") {
		t.Errorf("body carries the shared domain suffix:\n%s", body)
	}
}

func TestCloudPaneSaysWhenNoMigrationsAreActive(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: []Migration{
		{ID: 1, InstanceUUID: "a", Status: "completed", UpdatedAt: now.Add(-time.Minute)},
		{ID: 2, InstanceUUID: "b", Status: "failed", UpdatedAt: now.Add(-5 * time.Hour)},
	}})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(80, 8, now))
	if !strings.Contains(body, "no active migrations") {
		t.Errorf("want the idle message, got:\n%s", body)
	}
}

func TestCloudPaneMigrationsPlaceholderAndError(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")

	empty := newCloudPane(store.New(), "v1.31.4")
	if body := stripANSI(empty.renderMigrations(80, 6, now)); !strings.Contains(body, "polling migrations") {
		t.Errorf("want a polling placeholder before the first poll, got:\n%s", body)
	}

	s := store.New()
	s.Put(KeyMigrations, Migrations{Err: errors.New("nova is unreachable")})
	if body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(80, 6, now)); !strings.Contains(body, "nova is unreachable") {
		t.Errorf("want the poll error, got:\n%s", body)
	}
}

func TestCloudPaneRendersInventoryAtRest(t *testing.T) {
	s := store.New()
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Projects", Total: 42},
		{Label: "Servers", Total: 310, ByState: map[string]int{"ACTIVE": 300, "ERROR": 6, "SHUTOFF": 4}},
		{Label: "Load Balancers", Absent: true},
		{Label: "Volumes", Err: errors.New("cinder denied the request")},
	}})

	body := stripANSI(newResourcesPane(s).Render(80, 10, false))

	for _, want := range []string{
		"Projects:", "42",
		"Servers:", "310",
		"(ACTIVE 300, ERROR 6, SHUTOFF 4)",
		"Load Balancers: not deployed",
		"cinder denied",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestCloudPaneInventorySurvivesPartialFailure(t *testing.T) {
	s := store.New()
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Projects", Err: errors.New("keystone denied the request")},
		{Label: "Servers", Total: 12},
	}})

	body := stripANSI(renderInventory(s, 80, 10))
	if !strings.Contains(body, "Servers:") || !strings.Contains(body, "12") {
		t.Errorf("a working count was lost to a failing one:\n%s", body)
	}
}

func TestCloudPaneInventoryPlaceholderAndAuthError(t *testing.T) {
	if body := stripANSI(renderInventory(store.New(), 80, 6)); !strings.Contains(body, "polling OpenStack resources") {
		t.Errorf("want a polling placeholder before the first poll, got:\n%s", body)
	}

	s := store.New()
	s.Put(KeyInventory, Inventory{Err: errors.New("authenticate to cloud \"my-cloud\": bad credentials")})
	if body := stripANSI(renderInventory(s, 80, 6)); !strings.Contains(body, "bad credentials") {
		t.Errorf("want the auth error, got:\n%s", body)
	}
}

func TestCloudPaneRespectsBounds(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	s := store.New()
	items := make([]Migration, 0, 40)
	for i := range 40 {
		items = append(items, Migration{
			ID: int64(i), InstanceUUID: strings.Repeat("f", 32), Status: "running",
			Type: "live-migration", SourceCompute: "compute-node-1.site-a.example.com",
			DestCompute: "compute-node-2.site-a.example.com", UpdatedAt: now.Add(-time.Minute),
		})
	}
	s.Put(KeyMigrations, Migrations{Items: items})
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Projects", Total: 1},
		{Label: "Servers", Total: 2, ByState: map[string]int{"ACTIVE": 2}},
		{Label: "Networks", Total: 3},
		{Label: "Subnets", Total: 4},
		{Label: "Routers", Total: 5},
		{Label: "Floating IPs", Total: 6, ByState: map[string]int{"IN-USE": 4, "FREE": 2}},
		{Label: "Load Balancers", Total: 7, ByState: map[string]int{"ACTIVE": 7}},
		{Label: "Volumes", Total: 8, ByState: map[string]int{"in-use": 8}},
	}})

	for _, p := range []tui.Pane{newCloudPane(s, "v1.31.4"), newResourcesPane(s)} {
		mode := p.ID()
		for _, size := range [][2]int{{50, 5}, {80, 6}, {120, 12}} {
			w, h := size[0], size[1]
			body := p.Render(w, h, false)
			lines := strings.Split(body, "\n")
			if len(lines) > h {
				t.Errorf("mode %q at %dx%d: %d lines, want at most %d", mode, w, h, len(lines), h)
			}
			for i, ln := range lines {
				if got := len([]rune(stripANSI(ln))); got > w {
					t.Errorf("mode %q at %dx%d: line %d is %d wide\n%s", mode, w, h, i, got, ln)
				}
			}
		}
	}
}

func TestCloudPaneAppliesDedupAndWindow(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: []Migration{
		{ID: 1, InstanceUUID: "server-a", Status: "queued", UpdatedAt: now.Add(-10 * time.Minute)},
		{ID: 2, InstanceUUID: "server-a", Status: "running", UpdatedAt: now.Add(-time.Minute)},
		{ID: 3, InstanceUUID: "server-b", Status: "failed", UpdatedAt: now.Add(-5 * time.Hour)},
	}})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 10, now))
	if !strings.Contains(body, "1 active") || strings.Contains(body, "failed") {
		t.Errorf("want one active row and no stale failure:\n%s", body)
	}
	if strings.Contains(body, "queued") {
		t.Errorf("superseded attempt still shown:\n%s", body)
	}
}

func TestTrimErrKeepsOneLine(t *testing.T) {
	long := strings.Repeat("x", 200)
	if got := trimErr(long); len([]rune(got)) > 48 {
		t.Errorf("trimmed message is %d runes, want at most 48", len([]rune(got)))
	}
	if got := trimErr("short"); got != "short" {
		t.Errorf("trimErr(%q) = %q", "short", got)
	}
}

func TestAgeReportsUnknownForAZeroTimestamp(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	if got := age(now, time.Time{}); got != "?" {
		t.Errorf("age of a zero timestamp = %q, want ?", got)
	}
	if got := age(now, now.Add(-90*time.Second)); got == "?" {
		t.Errorf("a real timestamp rendered as unknown")
	}
}

func TestCloudPaneMigrationsFitOneGridColumn(t *testing.T) {
	const bodyWidth = 280/4 - 4

	now := time.Now()
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: []Migration{{
		ID:     1,
		Status: "post-migrating", Type: "live-migration",
		InstanceUUID:  "6f1c2b8e-4a3d-4f19-9c7e-2b8a5d1e0f34",
		SourceCompute: "compute-node-5.site-a.example.com",
		DestCompute:   "compute-node-6.site-a.example.com",
		UpdatedAt:     now.Add(-90 * time.Second),
	}}, UpdatedAt: now})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(bodyWidth, 10, now))
	for _, want := range []string{"compute-node-5", "compute-node-6"} {
		if !strings.Contains(body, want) {
			t.Errorf("host %q is truncated at %d columns; the destination is the\n"+
				"half an operator needs during a drain:\n%s", want, bodyWidth, body)
		}
	}
	for i, line := range strings.Split(body, "\n") {
		if got := lipgloss.Width(line); got > bodyWidth {
			t.Errorf("line %d is %d columns, want at most %d", i, got, bodyWidth)
		}
	}
}

func TestInventoryColorsFailingStates(t *testing.T) {
	s := store.New()
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Load Balancers", Total: 12, ByState: map[string]int{"ACTIVE": 3, "ERROR": 9}},
	}})

	body := renderInventory(s, 74, 6)

	if !strings.Contains(body, tui.StyleErr.Render("ERROR 9")) {
		t.Errorf("the ERROR state is not styled as one:\n%q", body)
	}
	if !strings.Contains(body, tui.StyleMuted.Render("ACTIVE 3")) {
		t.Errorf("a settled state should stay muted:\n%q", body)
	}
	plain := stripANSI(body)
	if !strings.Contains(plain, "(ERROR 9, ACTIVE 3)") {
		t.Errorf("breakdown is not (ERROR 9, ACTIVE 3): %q", plain)
	}
}

func TestInventoryBreakdownDropsRarestRatherThanTruncating(t *testing.T) {
	s := store.New()
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Load Balancers", Total: 4, ByState: map[string]int{"ACTIVE": 3, "ERROR": 1}},
		{Label: "Volumes", Total: 104, ByState: map[string]int{
			"IN-USE": 66, "AVAILABLE": 16, "RESERVED": 11,
			"ERROR_DELETING": 10, "ATTACHING": 1,
		}},
	}})

	const width = 74
	body := stripANSI(renderInventory(s, width, 6))

	for _, line := range strings.Split(body, "\n") {
		if lipgloss.Width(line) > width {
			t.Errorf("line overflows %d columns:\n%q", width, line)
		}
	}

	volumes := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Volumes:") {
			volumes = strings.TrimRight(line, " ")
		}
	}
	if volumes == "" {
		t.Fatalf("no Volumes line:\n%s", body)
	}
	for _, want := range []string{"104", "IN-USE 66", ")"} {
		if !strings.Contains(volumes, want) {
			t.Errorf("Volumes line missing %q: %q", want, volumes)
		}
	}
	if !strings.Contains(volumes, "+") {
		t.Errorf("dropped states are not counted: %q", volumes)
	}
	if strings.Contains(volumes, "ATTACHI") && !strings.Contains(volumes, "ATTACHING") {
		t.Errorf("a state name is truncated mid-word: %q", volumes)
	}
}

func TestCloudPaneSummarizesUnresolvedAndEnumeratesWhenTall(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const stale = "bbbbbbbb-2222-3333-4444-555555555555"
	s := store.New()
	s.Put(KeyMigrations, Migrations{
		Items: []Migration{
			{ID: 1, InstanceUUID: "aaaaaaaa-1111-2222-3333-444444444444", Status: "running",
				SourceCompute: "compute-node-1.site-a.example.com",
				DestCompute:   "compute-node-2.site-a.example.com",
				CreatedAt:     now.Add(-2 * time.Minute), UpdatedAt: now.Add(-time.Minute)},
			{ID: 2, InstanceUUID: stale, Status: "error",
				SourceCompute: "compute-node-3.site-a.example.com",
				CreatedAt:     now.Add(-73 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour)},
		},
		BrokenKnown: true,
		Broken: map[string]BrokenServer{
			stale: {UUID: stale, Host: "compute-node-9.site-a.example.com",
				Fault: "Live migration operation failed"},
		},
	})
	pane := newCloudPane(s, "v1.31.4")

	compact := stripANSI(pane.renderMigrations(90, 5, now))
	if !strings.Contains(compact, "1 unresolved") {
		t.Errorf("compact pane does not name the backlog:\n%s", compact)
	}
	if strings.Contains(compact, "Unresolved failures") {
		t.Errorf("compact pane spent rows enumerating the backlog:\n%s", compact)
	}

	zoomed := stripANSI(pane.renderMigrations(90, 30, now))
	for _, want := range []string{
		"Unresolved failures (1)",
		"bbbbbbbb",
		"compute-node-9",
		"Live migration operation failed",
	} {
		if !strings.Contains(zoomed, want) {
			t.Errorf("zoomed pane missing %q:\n%s", want, zoomed)
		}
	}
}

func TestCloudPanePromotesUnresolvedOnADrainingHost(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const stale = "bbbbbbbb-2222-3333-4444-555555555555"
	const host = "compute-node-9.site-a.example.com"
	items := []Migration{
		{ID: 2, InstanceUUID: stale, Status: "error",
			SourceCompute: "compute-node-3.site-a.example.com",
			CreatedAt:     now.Add(-73 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour)},
	}
	broken := map[string]BrokenServer{stale: {UUID: stale, Host: host}}

	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: items, Broken: broken, BrokenKnown: true})
	quiet := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 8, now))
	if !strings.Contains(quiet, "1 unresolved") || strings.Contains(quiet, "1 failed") {
		t.Errorf("nobody is draining, so it should only be counted:\n%s", quiet)
	}

	s.Put(KeyMigrations, Migrations{Items: items, Broken: broken, BrokenKnown: true,
		Draining: map[string]bool{host: true}})
	draining := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 8, now))
	if !strings.Contains(draining, "1 failed") || strings.Contains(draining, "unresolved") {
		t.Errorf("its host is draining, so it should be a row:\n%s", draining)
	}
	if !strings.Contains(draining, "bbbbbbbb") {
		t.Errorf("promoted row does not name the server:\n%s", draining)
	}
}

func TestCloudPaneKeepsActivesVisibleUnderAFloodOfFailures(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	items := make([]Migration, 0, 12)
	for i := range 10 {
		items = append(items, Migration{
			ID: int64(100 + i), InstanceUUID: strings.Repeat(string(rune('a'+i)), 8),
			Status: "failed", SourceCompute: "compute-node-1.site-a.example.com",
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		})
	}
	items = append(items, Migration{
		ID: 200, InstanceUUID: "99999999-aaaa-bbbb-cccc-dddddddddddd", Status: "running",
		SourceCompute: "compute-node-2.site-a.example.com",
		DestCompute:   "compute-node-3.site-a.example.com",
		CreatedAt:     now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	})
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: items})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 8, now))
	if !strings.Contains(body, "99999999") {
		t.Errorf("the in-flight migration was evicted by the failure backlog:\n%s", body)
	}
	if !strings.Contains(body, "10 failed") {
		t.Errorf("summary undercounts the failures it did not draw:\n%s", body)
	}
}

func TestCloudPaneContentHeightExcludesTheBacklog(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const stale = "bbbbbbbb-2222-3333-4444-555555555555"
	s := store.New()
	s.Put(KeyMigrations, Migrations{
		Items: []Migration{
			{ID: 1, InstanceUUID: "aaaaaaaa", Status: "running",
				CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
			{ID: 2, InstanceUUID: stale, Status: "error",
				CreatedAt: now.Add(-73 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour)},
		},
		BrokenKnown: true,
		Broken:      map[string]BrokenServer{stale: {UUID: stale, Host: "compute-node-9"}},
	})

	if got := newCloudPane(s, "v1.31.4").ContentHeight(90); got != 3 {
		t.Errorf("ContentHeight = %d, want 3 (one listed row, not the backlog)", got)
	}
}

func TestMigrationStyleSeparatesBrokenFromRecovered(t *testing.T) {
	broken := migrationStyle("error", true).Render("x")
	recovered := migrationStyle("error", false).Render("x")
	if broken == recovered {
		t.Error("a still-broken failure renders identically to a recovered one")
	}
	if plain := migrationStyle("completed", false).Render("x"); plain == broken {
		t.Error("a still-broken failure renders like an unremarkable row")
	}
}

func TestCloudPaneShowsDrainProgress(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const host = "compute-node-3.site-a.example.com"
	s := store.New()
	s.Put(KeyMigrations, Migrations{
		Items: []Migration{{ID: 1, InstanceUUID: "aaaaaaaa", Status: "migrating",
			SourceCompute: host, DestCompute: "compute-node-1.site-a.example.com",
			CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}},
		Draining: map[string]bool{host: true},
		Drains:   []Drain{{Host: host, Remaining: 12, Moving: 3, Stuck: 1}},
	})

	pane := newCloudPane(s, "v1.31.4")

	headed := stripANSI(pane.renderMigrations(90, 12, now))
	for _, want := range []string{"Draining", "compute-node-3", "12 left", "3 moving", "1 stuck"} {
		if !strings.Contains(headed, want) {
			t.Errorf("drain block missing %q:\n%s", want, headed)
		}
	}
	lines := strings.Split(headed, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[2]) != "" {
		t.Errorf("no blank line between the drain block and the migrations:\n%s", headed)
	}
	if !strings.HasPrefix(lines[1], "    ") {
		t.Errorf("host line is not indented under the heading: %q", lines[1])
	}

	compact := stripANSI(pane.renderMigrations(90, 5, now))
	if !strings.Contains(compact, "draining compute-node-3") {
		t.Errorf("compact form lost its label:\n%s", compact)
	}
	if strings.Contains(compact, "Draining\n") {
		t.Errorf("compact form spent a line on the heading:\n%s", compact)
	}
	if cl := strings.Split(compact, "\n"); len(cl) < 2 || strings.TrimSpace(cl[1]) != "" {
		t.Errorf("compact form dropped the separator:\n%s", compact)
	}
}

func TestCloudPaneDistinguishesFinishedFromStalledDrain(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const host = "compute-node-3.site-a.example.com"

	for _, tc := range []struct {
		name  string
		drain Drain
		want  string
		notW  string
	}{
		{"finished", Drain{Host: host, Remaining: 0}, "empty", "left"},
		{"stalled", Drain{Host: host, Remaining: 7}, "none moving", "empty"},
		{"unreadable", Drain{Host: host, Err: errors.New("nope")}, "unavailable", "left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store.New()
			s.Put(KeyMigrations, Migrations{
				Draining: map[string]bool{host: true},
				Drains:   []Drain{tc.drain},
			})
			body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 8, now))
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, body)
			}
			if strings.Contains(body, tc.notW) {
				t.Errorf("did not want %q in:\n%s", tc.notW, body)
			}
		})
	}
}

func TestCloudPaneKeepsDrainBlockWithNoMigrations(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	const host = "compute-node-3.site-a.example.com"
	s := store.New()
	s.Put(KeyMigrations, Migrations{
		Draining: map[string]bool{host: true},
		Drains:   []Drain{{Host: host, Remaining: 7}},
	})

	pane := newCloudPane(s, "v1.31.4")
	body := stripANSI(pane.renderMigrations(90, 8, now))
	if strings.Contains(body, "no migrations") && !strings.Contains(body, "in flight") {
		t.Errorf("idle placeholder hid the drain block:\n%s", body)
	}
	if !strings.Contains(body, "7 left") {
		t.Errorf("drain progress missing:\n%s", body)
	}
	if got := pane.ContentHeight(90); got < 2 {
		t.Errorf("ContentHeight = %d, too short for the block plus its note", got)
	}
}

func TestCloudPaneCountsUnprobedDrains(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	draining := map[string]bool{}
	for i := range 11 {
		draining[fmt.Sprintf("compute-node-%d.site-a.example.com", i)] = true
	}
	// maxDrains is 8 in the openstack package; simulate 11 draining hosts
	// with only 8 probed.
	const maxDrains = 8
	drains := make([]Drain, 0, maxDrains)
	for i := range maxDrains {
		drains = append(drains, Drain{
			Host:      fmt.Sprintf("compute-node-%d.site-a.example.com", i),
			Remaining: 2, Moving: 1,
		})
	}
	s := store.New()
	s.Put(KeyMigrations, Migrations{Draining: draining, Drains: drains})

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 30, now))
	if !strings.Contains(body, "+ 3 more draining") {
		t.Errorf("unprobed drains not accounted for:\n%s", body)
	}
}

func TestFailureBudgetLeavesRoomForActives(t *testing.T) {
	tests := []struct {
		failures, available, want int
	}{
		{failures: 2, available: 8, want: 2},
		{failures: 4, available: 8, want: 4},
		{failures: 8, available: 8, want: 4},
		{failures: 9, available: 3, want: 1},
		{failures: 3, available: 1, want: 1},
		{failures: 5, available: 0, want: 0},
		{failures: 0, available: 6, want: 0},
	}
	for _, tc := range tests {
		if got := failureBudget(tc.failures, tc.available); got != tc.want {
			t.Errorf("failureBudget(%d, %d) = %d, want %d",
				tc.failures, tc.available, got, tc.want)
		}
	}
}
