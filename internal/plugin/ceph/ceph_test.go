package ceph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
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
