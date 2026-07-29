package ceph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
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

// --- parsing --------------------------------------------------------------

func TestParseStatus_AllFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures — the parser would be untested")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			if _, err := ParseStatus(fixture(t, e.Name())); err != nil {
				t.Fatalf("does not parse: %v", err)
			}
		})
	}
}

// Reduced from a real production cluster. The notable part is the summary-only
// mgrmap: `available` and `num_standbys` are present with no `active_name`.
func TestParseStatus_RealHealthyCluster(t *testing.T) {
	got, err := ParseStatus(fixture(t, "reef-healthy-summary-mgrmap.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !got.HealthOK() {
		t.Errorf("Health: got %q want HEALTH_OK", got.Health)
	}
	if len(got.Checks) != 0 {
		t.Errorf("a healthy cluster should report no checks: %+v", got.Checks)
	}
	if !got.Mons.Healthy() || got.Mons.InQuorum != 3 || got.Mons.Total != 3 {
		t.Errorf("Mons: got %+v want 3/3", got.Mons)
	}

	// The manager is available but unnamed, which is a gap in the data rather
	// than a missing manager. Rendering it as "no active manager" would be a
	// false alarm on a perfectly healthy cluster.
	if !got.Mgr.Available {
		t.Error("Mgr should be available")
	}
	if !got.Mgr.ActiveUnknown() {
		t.Error("a summary-only mgrmap should report the active name as unknown")
	}
	if got.Mgr.Standbys != 2 {
		t.Errorf("Standbys: got %d want 2", got.Mgr.Standbys)
	}

	if !got.OSDs.Healthy() || got.OSDs.Total != 36 || got.OSDs.Up != 36 {
		t.Errorf("OSDs: got %+v want 36/36", got.OSDs)
	}
	if !got.PGs.AllClean() || got.PGs.Total != 1137 {
		t.Errorf("PGs: got total=%d clean=%d", got.PGs.Total, got.PGs.CleanPGs())
	}
	if got.PGs.Objects != 5659981 || got.PGs.Pools != 27 {
		t.Errorf("PGs: got %d objects in %d pools", got.PGs.Objects, got.PGs.Pools)
	}

	// Raw usage includes replication, so it is much larger than stored data — and
	// it is raw usage that fills the cluster.
	if got.PGs.UsedPercent() != 12 {
		t.Errorf("UsedPercent: got %d want 12", got.PGs.UsedPercent())
	}
	if got.PGs.DataBytes >= got.PGs.UsedBytes {
		t.Error("stored data should be less than raw usage on a replicated cluster")
	}

	if got.IO.ReadOpsPerSec != 975 || got.IO.WriteOpsPerSec != 1351 {
		t.Errorf("IO: got %+v", got.IO)
	}
	if len(got.Unreadable) != 0 {
		t.Errorf("nothing should be unreadable: %v", got.Unreadable)
	}
}

// A named active manager is used as-is, with no follow-up query needed.
func TestParseStatus_NamedActiveManager(t *testing.T) {
	got, err := ParseStatus(fixture(t, "degraded-with-checks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Mgr.Active != "mgr-a" {
		t.Errorf("Active: got %q want mgr-a", got.Mgr.Active)
	}
	if got.Mgr.ActiveUnknown() {
		t.Error("a named manager should not report unknown")
	}
}

// "HEALTH_WARN" alone gives nothing to act on, so the checks are named — and
// sorted, since a map's iteration order would reshuffle the pane every render.
func TestParseStatus_ChecksAreNamedAndSorted(t *testing.T) {
	got, err := ParseStatus(fixture(t, "degraded-with-checks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("Checks: got %d want 2", len(got.Checks))
	}
	if got.Checks[0].Name != "OSD_NEARFULL" || got.Checks[1].Name != "PG_DEGRADED" {
		t.Errorf("checks should be name-sorted: got %q, %q", got.Checks[0].Name, got.Checks[1].Name)
	}
	if !strings.Contains(got.Checks[0].Message, "nearfull") {
		t.Errorf("the message should survive: %q", got.Checks[0].Message)
	}
	// A muted check is a suppressed warning, so it is counted rather than ignored.
	if got.MutedChecks != 1 {
		t.Errorf("MutedChecks: got %d want 1", got.MutedChecks)
	}

	// Stable across repeated parses.
	for range 20 {
		again, _ := ParseStatus(fixture(t, "degraded-with-checks.json"))
		if again.Checks[0].Name != got.Checks[0].Name {
			t.Fatal("check order is not deterministic")
		}
	}
}

func TestParseStatus_DegradedCluster(t *testing.T) {
	got, err := ParseStatus(fixture(t, "degraded-with-checks.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A monitor out of quorum is one step from losing the control plane.
	if got.Mons.Healthy() {
		t.Errorf("2 of 3 mons is not healthy: %+v", got.Mons)
	}
	if got.OSDs.Healthy() {
		t.Errorf("34 of 36 OSDs up is not healthy: %+v", got.OSDs)
	}
	if got.PGs.AllClean() {
		t.Error("not every PG is clean")
	}
	if got.PGs.CleanPGs() != 1100 {
		t.Errorf("CleanPGs: got %d want 1100", got.PGs.CleanPGs())
	}
	// 124440414466867 of 138267127185408 is 89.99…%, and integer division
	// truncates. Truncating is the right direction for a capacity figure: it
	// never overstates how full the cluster is.
	if got.PGs.UsedPercent() != 89 {
		t.Errorf("UsedPercent: got %d want 89", got.PGs.UsedPercent())
	}
}

// A wrongly typed section costs only itself, as with the Cilium parser.
func TestParseStatus_BrokenSectionIsIsolated(t *testing.T) {
	got, err := ParseStatus(fixture(t, "broken-section.json"))
	if err != nil {
		t.Fatalf("should still parse: %v", err)
	}
	if !got.HealthOK() {
		t.Error("health should still decode")
	}
	if got.PGs.Total != 10 {
		t.Errorf("pgmap should still decode: got %d", got.PGs.Total)
	}
	if got.OSDs.Total != 0 {
		t.Errorf("the broken section should be zero: %+v", got.OSDs)
	}
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "osdmap" {
		t.Errorf("Unreadable: got %v want [osdmap]", got.Unreadable)
	}
}

func TestParseStatus_MalformedJSON(t *testing.T) {
	if _, err := ParseStatus([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseMgrStat(t *testing.T) {
	got, err := ParseMgrStat([]byte(`{"epoch":42,"active_name":"mgr-b","num_standby":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "mgr-b" {
		t.Errorf("got %q want mgr-b", got)
	}
	if _, err := ParseMgrStat([]byte("nope")); err == nil {
		t.Error("expected an error for malformed json")
	}
}

// --- derived state --------------------------------------------------------

func TestUsedPercent_UnknownTotal(t *testing.T) {
	if got := (PGs{UsedBytes: 100}).UsedPercent(); got != -1 {
		t.Errorf("got %d want -1 when the total is unknown", got)
	}
}

func TestMgrActiveUnknown(t *testing.T) {
	// Unavailable with no name is a missing manager, not unknown data.
	if (Mgr{}).ActiveUnknown() {
		t.Error("an unavailable manager is not 'unknown'")
	}
	if !(Mgr{Available: true}).ActiveUnknown() {
		t.Error("available with no name should be unknown")
	}
	if (Mgr{Available: true, Active: "mgr-a"}).ActiveUnknown() {
		t.Error("a named manager is not unknown")
	}
}

// --- settings -------------------------------------------------------------

func TestSettingsFrom(t *testing.T) {
	if got := SettingsFrom(nil); got != Defaults() {
		t.Errorf("got %+v want defaults", got)
	}
	got := SettingsFrom(map[string]any{"namespace": "storage", "tools_selector": "app=tools"})
	if got.Namespace != "storage" || got.ToolsSelector != "app=tools" {
		t.Errorf("got %+v", got)
	}
}

// Everything is left to discovery, following the pattern the earlier plugins
// arrived at the hard way.
func TestDefaults_AreEmpty(t *testing.T) {
	if Defaults() != (Settings{}) {
		t.Errorf("got %+v want everything derived", Defaults())
	}
}

// --- banner ---------------------------------------------------------------

func stateFrom(t *testing.T, name string) State {
	t.Helper()
	status, err := ParseStatus(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return State{Tier: kube.TierFull, Status: status, Pod: "rook-ceph-tools-abc", UpdatedAt: time.Now()}
}

func TestCells(t *testing.T) {
	s := store.New()

	s.Put(KeyState, stateFrom(t, "reef-healthy-summary-mgrmap.json"))
	if got := (&Plugin{}).Cells(s)[0]; got.Status != tui.BannerOK {
		t.Errorf("healthy: got %v %q", got.Status, got.Detail)
	}

	s.Put(KeyState, stateFrom(t, "degraded-with-checks.json"))
	cell := (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerWarn {
		t.Errorf("degraded: got %v want warn", cell.Status)
	}
	// The cell names the check driving it, so it says what is wrong rather than
	// only that something is.
	if cell.Detail != "OSD_NEARFULL" {
		t.Errorf("detail should name the check: got %q", cell.Detail)
	}
}

// Ceph can report OK while an OSD is out, if the data is still replicated. That
// is worth a warning, since capacity and redundancy are both reduced.
func TestCells_OSDDownDespiteHealthOK(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{
		Tier: kube.TierFull,
		Status: Status{
			Health: "HEALTH_OK",
			OSDs:   OSDs{Total: 36, Up: 35, In: 36},
		},
		UpdatedAt: time.Now(),
	})
	cell := (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerWarn {
		t.Errorf("got %v want warn", cell.Status)
	}
	if !strings.Contains(cell.Detail, "35/36") {
		t.Errorf("detail should report the OSD count: %q", cell.Detail)
	}
}

func TestCells_NoStateYet(t *testing.T) {
	if got := (&Plugin{}).Cells(store.New()); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

// --- pane -----------------------------------------------------------------

func TestPane_HealthyCluster(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "reef-healthy-summary-mgrmap.json"))
	body := stripANSI(newPane(s).Render(80, 12, false))

	for _, want := range []string{"HEALTH_OK", "3/3 in quorum", "36/36 up", "active+clean"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// Raw usage is what fills the cluster, so it is what the percentage reports.
	if !strings.Contains(body, "12%") {
		t.Errorf("expected raw usage percentage:\n%s", body)
	}
	// And stored data is shown alongside, since the two differ by replication.
	if !strings.Contains(body, "stored") {
		t.Errorf("expected stored data alongside raw usage:\n%s", body)
	}
	// An unnamed manager reads as a gap in the data, never as a missing manager.
	if !strings.Contains(body, "name not reported") {
		t.Errorf("expected the unknown manager name to be explicit:\n%s", body)
	}
	if strings.Contains(body, "no active manager") {
		t.Errorf("an available manager must not read as missing:\n%s", body)
	}
}

func TestPane_DegradedNamesTheChecks(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "degraded-with-checks.json"))
	body := stripANSI(newPane(s).Render(100, 16, false))

	for _, want := range []string{"HEALTH_WARN", "OSD_NEARFULL", "PG_DEGRADED", "nearfull"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// A muted check is a suppressed warning, so an OK-looking cluster with mutes
	// is not an unqualified OK.
	if !strings.Contains(body, "muted") {
		t.Errorf("expected the muted count:\n%s", body)
	}
	// Unclean placement groups are named, which says whether it is recovering or
	// stuck.
	if !strings.Contains(body, "undersized") {
		t.Errorf("expected the unclean PG states named:\n%s", body)
	}
}

func TestPane_States(t *testing.T) {
	if got := stripANSI(newPane(store.New()).Render(60, 8, false)); !strings.Contains(got, "loading") {
		t.Errorf("got %q want loading", got)
	}

	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierInformer, TierReason: "no pods/exec permission"})
	if got := stripANSI(newPane(s).Render(70, 8, false)); !strings.Contains(got, "pods/exec") {
		t.Errorf("a reduced tier should say why: %q", got)
	}

	s = store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Err: errFake{}})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "boom") {
		t.Errorf("got %q want the error", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_RespectsBounds(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "degraded-with-checks.json"))
	p := newPane(s)

	for _, w := range []int{20, 44, 90, 220} {
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

func TestBytesAndCount(t *testing.T) {
	byteTests := map[int64]string{
		512:             "512B",
		2048:            "2.0K",
		5 << 20:         "5.0M",
		3 << 30:         "3.0G",
		138267127185408: "125.8T",
	}
	for in, want := range byteTests {
		if got := bytes(in); got != want {
			t.Errorf("bytes(%d): got %q want %q", in, got, want)
		}
	}

	countTests := map[int64]string{
		42:      "42",
		1500:    "1.5k",
		5659981: "5.7M",
	}
	for in, want := range countTests {
		if got := count(in); got != want {
			t.Errorf("count(%d): got %q want %q", in, got, want)
		}
	}
}

// Ceph reports through the overview whenever the plugin is active, healthy or
// not — it no longer has a pane, so silence here would mean silence everywhere but
// the banner. What varies is the content, not the presence.
func TestSummary_PresentWhenHealthy(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Status: Status{
		Health: "HEALTH_OK",
		Mons:   Mons{Total: 3, InQuorum: 3},
		OSDs:   OSDs{Total: 30, Up: 30, In: 30},
		PGs:    PGs{Total: 100, ByState: []PGState{{Name: "active+clean", Count: 100}}},
	}})
	block, want := summaryBlock(s)
	if !want {
		t.Fatal("an active Ceph should always contribute a block")
	}
	body := stripANSI(strings.Join(block.Lines, "\n"))
	for _, expect := range []string{"HEALTH_OK", "30/30 up", "mons 3/3", "100/100 clean"} {
		if !strings.Contains(body, expect) {
			t.Errorf("block should report %q:\n%s", expect, body)
		}
	}
	// Three lines, because a contributed block must not lengthen the overview row.
	if len(block.Lines) > 3 {
		t.Errorf("block has %d lines, want at most 3", len(block.Lines))
	}
}

func TestSummary_NamesTheFailingCheck(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Status: Status{
		Health: "HEALTH_WARN",
		Mons:   Mons{Total: 3, InQuorum: 3},
		OSDs:   OSDs{Total: 408, Up: 398, In: 398},
		PGs:    PGs{UsedBytes: 10, TotalBytes: 100},
		Checks: []Check{{Name: "OSD_DOWN", Message: "10 osds down"}},
	}})
	block, want := summaryBlock(s)
	if !want {
		t.Fatal("a degraded Ceph should contribute a block")
	}
	body := stripANSI(strings.Join(block.Lines, "\n"))
	// The check name rides on the health line: it is the reason the reader's eye
	// should stop here, so it must survive the three-line budget.
	for _, expect := range []string{"HEALTH_WARN", "OSD_DOWN", "398/408", "10% used"} {
		if !strings.Contains(body, expect) {
			t.Errorf("block should report %q:\n%s", expect, body)
		}
	}
	if len(block.Lines) > 3 {
		t.Errorf("block has %d lines, want at most 3", len(block.Lines))
	}
}

// Without exec the pane used to say so in its body; the block says it in one line
// rather than claiming the column and showing nothing.
func TestSummary_SaysWhenDetailIsUnavailable(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{
		Tier:       kube.TierInformer,
		TierReason: "no pods/exec permission on rook-ceph",
		Status:     Status{Health: "HEALTH_OK"},
	})
	block, want := summaryBlock(s)
	if !want {
		t.Fatal("a present but unreadable Ceph should still contribute")
	}
	body := stripANSI(strings.Join(block.Lines, "\n"))
	if !strings.Contains(body, "no detail") || !strings.Contains(body, "pods/exec") {
		t.Errorf("block should explain the missing detail:\n%s", body)
	}
}

// Nothing published yet must stay silent: a column claiming space to say
// "loading" has not earned it, and the banner already covers an unread subsystem.
func TestSummary_SilentWithoutData(t *testing.T) {
	if _, want := summaryBlock(store.New()); want {
		t.Error("an empty store should contribute no block")
	}
}

// Ceph contributes no grid pane — that column is what the OpenStack pane grew
// into. The renderer stays under test so the decision remains reversible.
func TestPanes_CephHasNoGridPane(t *testing.T) {
	if panes := (&Plugin{}).Panes(store.New()); len(panes) != 0 {
		t.Errorf("got %d panes, want none: Ceph reports through the overview", len(panes))
	}
}
