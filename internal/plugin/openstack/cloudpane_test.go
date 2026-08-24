package openstack

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/gophercloud/gophercloud/v2"

	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// Migrations are shown whoever is draining, so the title no longer depends on
// whether Cluster API happens to be rolling.
func TestCloudPaneTitleIsStable(t *testing.T) {
	s := store.New()
	for _, target := range []string{"", "v1.31.4"} {
		if got := newCloudPane(s, target).Title(); got != "Server Migrations" {
			t.Errorf("target %q: title = %q, want Server Migrations", target, got)
		}
	}
}

// The plugin contributes four panes, and their IDs have to differ or one becomes
// unreachable by focus and jump keys.
func TestPluginContributesDistinctPanes(t *testing.T) {
	panes := New(Settings{Cloud: "my-cloud", TargetVersion: "v1.31.4"}).Panes(store.New())
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
	// Everything but migrations belongs to the Cloud frame; migrations stands
	// alone, because it needs a full column of its own during a drain.
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
		"abcdef12",                        // shortened UUID, greppable against server list
		"compute-node-1 → compute-node-2", // hosts trimmed to their first DNS label
		"compute-node-3 → ?",              // no destination chosen yet
		"running", "failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// The FQDN suffix is identical on every row, so it must not be there.
	if strings.Contains(body, "site-a.example.com") {
		t.Errorf("body carries the shared domain suffix:\n%s", body)
	}
}

// An empty list during a rollout is good news — the drain is not stuck — and
// saying so beats an empty table.
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
		// Most common state first, so the eye lands on the bulk without reading
		// the whole parenthesis.
		"(ACTIVE 300, ERROR 6, SHUTOFF 4)",
		// An absent service is a configuration fact, not a failure.
		"Load Balancers: not deployed",
		"cinder denied",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// One kind failing costs one line, not the pane: that is the whole point of a
// per-kind error.
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

	// An authentication failure is pane-wide: nothing could have been counted.
	s := store.New()
	s.Put(KeyInventory, Inventory{Err: errors.New("authenticate to cloud \"my-cloud\": bad credentials")})
	if body := stripANSI(renderInventory(s, 80, 6)); !strings.Contains(body, "bad credentials") {
		t.Errorf("want the auth error, got:\n%s", body)
	}
}

// Rendering must respect the box it is given; the renderer clips rather than
// wraps, so a pane that overflows corrupts the grid.
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

// The dedup and the recency window belong to the display, so the pane must apply
// them to whatever raw history the poll published.
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

func TestNotDeployedRecognisesAMissingService(t *testing.T) {
	// Octavia absent from the catalog is what this looks like in practice.
	if !notDeployed(gophercloud.ErrEndpointNotFound{}) {
		t.Error("ErrEndpointNotFound not recognized as an absent service")
	}
	if !notDeployed(errors.Join(errors.New("wrapped"), gophercloud.ErrEndpointNotFound{})) {
		t.Error("wrapped ErrEndpointNotFound not recognized")
	}
	if notDeployed(errors.New("connection refused")) {
		t.Error("an ordinary failure was reported as an absent service")
	}
	if notDeployed(nil) {
		t.Error("nil reported as an absent service")
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

// The migrations table has to fit a full pair of compute hostnames in one grid
// column of four, which is where the operator will be reading it during a drain.
//
// This is pinned with a test rather than left to the demo fixture, because a
// fixture's hostnames are whatever someone typed. Short ones make the layout look
// comfortable while it truncates the destination on the cluster that matters, and
// the destination is the half that carries the news: during a drain the source is
// the hypervisor you are draining and you already know it.
//
// Keep these fixture hostnames at 14 characters before the domain. That is the
// width the column was calibrated against, and shortening them to make room would
// retire the check rather than satisfy it.
func TestCloudPaneMigrationsFitOneGridColumn(t *testing.T) {
	// 280 columns over four grid columns, less the pane's own chrome.
	const bodyWidth = 280/4 - 4

	now := time.Now()
	s := store.New()
	s.Put(KeyMigrations, Migrations{Items: []Migration{{
		ID: 1,
		// The longest status Nova reports, so the test measures the worst case
		// rather than the common one.
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
	// And every line still fits the column it was given.
	for i, line := range strings.Split(body, "\n") {
		if got := lipgloss.Width(line); got > bodyWidth {
			t.Errorf("line %d is %d columns, want at most %d", i, got, bodyWidth)
		}
	}
}

// Cinder reports more states than a one-column frame can hold, and the line has
// to lose the rarest rather than lose the end of a word.
//
// Taken from a live cloud, where the pane rendered
// "... ERROR_DELETING 10, ATTACHI" hard against the border: a state that does not
// exist, followed by however many the reader could not know were there. Resources
// moved from a two-column pane into a section of the Cloud frame, which halved
// its width, and the breakdown had always been written in full and clipped.
func TestInventoryBreakdownDropsRarestRatherThanTruncating(t *testing.T) {
	s := store.New()
	s.Put(KeyInventory, Inventory{Counts: []Count{
		{Label: "Load Balancers", Total: 4, ByState: map[string]int{"ACTIVE": 3, "ERROR": 1}},
		{Label: "Volumes", Total: 104, ByState: map[string]int{
			"IN-USE": 66, "AVAILABLE": 16, "RESERVED": 11,
			"ERROR_DELETING": 10, "ATTACHING": 1,
		}},
	}})

	const width = 74 // one grid column of four, less the frame's chrome
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
	// The bulk state survives, the parenthesis is closed, and the states that
	// did not fit are counted rather than silently gone.
	for _, want := range []string{"104", "IN-USE 66", ")"} {
		if !strings.Contains(volumes, want) {
			t.Errorf("Volumes line missing %q: %q", want, volumes)
		}
	}
	if !strings.Contains(volumes, "+") {
		t.Errorf("dropped states are not counted: %q", volumes)
	}
	// The specific failure this replaced: a state name cut mid-word.
	if strings.Contains(volumes, "ATTACHI") && !strings.Contains(volumes, "ATTACHING") {
		t.Errorf("a state name is truncated mid-word: %q", volumes)
	}
}

// The summary names an unresolved backlog; zoom is what enumerates it.
//
// The grid caps this pane at ContentHeight, which asks only for the compact
// form, while a zoomed pane is handed the whole body — so "has rows to spare"
// and "the operator pressed z" are the same condition from here.
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
			// Three days old, so only the retention rule keeps it at all.
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
		"bbbbbbbb",                        // the server
		"compute-node-9",                  // where it actually is, not where it was going
		"Live migration operation failed", // Nova's own reason
	} {
		if !strings.Contains(zoomed, want) {
			t.Errorf("zoomed pane missing %q:\n%s", want, zoomed)
		}
	}
}

// A retained failure earns a row back the moment someone drains the host its
// instance is stuck on — that is the landmine going off.
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

// Failures sort first, so a backlog of them must not push the drain the
// operator is watching off a short pane.
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
	// The summary still tells the truth about how many there are.
	if !strings.Contains(body, "10 failed") {
		t.Errorf("summary undercounts the failures it did not draw:\n%s", body)
	}
}

// ContentHeight asks for the compact form only. Asking for the backlog too
// would hand the pane a tall grid cell and there would be nothing left for zoom
// to reveal.
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

	// One row, plus the summary and the table header.
	if got := newCloudPane(s, "v1.31.4").ContentHeight(90); got != 3 {
		t.Errorf("ContentHeight = %d, want 3 (one listed row, not the backlog)", got)
	}
}

// The two kinds of failure used to look identical. A server that is down and
// cannot be moved is not the same event as a drain that did not complete.
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

// The drain block is the frame the migration table is read inside: which host
// is being emptied, how far it has got, and whether anything is moving.
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

	body := stripANSI(newCloudPane(s, "v1.31.4").renderMigrations(90, 10, now))
	for _, want := range []string{"draining", "compute-node-3", "12 left", "3 moving", "1 stuck"} {
		if !strings.Contains(body, want) {
			t.Errorf("drain block missing %q:\n%s", want, body)
		}
	}
}

// The two states a migration table cannot tell apart. An empty table means
// "finished" and "stalled" alike, so the block has to say which.
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

// A drain with nothing in flight publishes no migration rows, and that is
// exactly when the block is the only thing worth drawing. The pane must not
// fall through to its idle placeholder and hide it.
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
	// And the pane must ask for the height it needs to draw it.
	if got := pane.ContentHeight(90); got < 2 {
		t.Errorf("ContentHeight = %d, too short for the block plus its note", got)
	}
}

// Hosts past the probe cap are counted, never silently dropped — the block must
// not imply the cloud has fewer drains running than it has.
func TestCloudPaneCountsUnprobedDrains(t *testing.T) {
	now := at(t, "2026-07-28T12:00:00Z")
	draining := map[string]bool{}
	for i := range 11 {
		draining[fmt.Sprintf("compute-node-%d.site-a.example.com", i)] = true
	}
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
