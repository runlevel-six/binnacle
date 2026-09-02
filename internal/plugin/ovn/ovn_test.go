package ovn

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

// fixture reads captured `ovn-appctl cluster/status` output.
//
// ovn-appctl has no structured output mode, so the text *is* the contract.
// Fixtures are the only way to notice it changing.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
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

func TestParseClusterStatus_AllFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures — the parser would be untested")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			got, err := ParseClusterStatus(fixture(t, e.Name()))
			if err != nil {
				t.Fatalf("does not parse: %v", err)
			}
			if got.Database == "" {
				t.Error("Database should be populated in every fixture")
			}
			if len(got.Servers) == 0 {
				t.Error("Servers should be populated in every fixture")
			}
		})
	}
}

// The failure this plugin exists to catch, read from the only view that can see it:
// the leader reports a member last heard from 7,965,698 ms — 2.2 hours — ago, while
// its StatefulSet reports 3/3 Ready the whole time.
func TestParseClusterStatus_LeaderWithStaleMember(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-leader-with-stale-member.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if got.Database != "OVN_Northbound" {
		t.Errorf("Database: got %q", got.Database)
	}
	// The short ID is what every other line refers to, so that is what is kept.
	if got.ClusterID != "e715" || got.ServerID != "cc1b" {
		t.Errorf("ids: got cluster=%q server=%q want e715/cc1b", got.ClusterID, got.ServerID)
	}
	if got.Role != "leader" || got.Term != 317 || got.Leader != "self" {
		t.Errorf("raft: got role=%q term=%d leader=%q", got.Role, got.Term, got.Leader)
	}
	if got.Status != "cluster member" {
		t.Errorf("Status: got %q", got.Status)
	}
	if got.LogLow != 917750 || got.LogHigh != 924823 {
		t.Errorf("Log: got [%d, %d]", got.LogLow, got.LogHigh)
	}
	if got.Uncommitted != 0 || got.Unapplied != 0 {
		t.Errorf("entries: got %d uncommitted %d unapplied", got.Uncommitted, got.Unapplied)
	}
	if got.Disconnections != 13 {
		t.Errorf("Disconnections: got %d want 13", got.Disconnections)
	}

	if len(got.Servers) != 3 {
		t.Fatalf("Servers: got %d want 3", len(got.Servers))
	}
	byID := map[string]Server{}
	for _, s := range got.Servers {
		byID[s.ID] = s
	}

	// The healthy follower.
	if s := byID["8469"]; !s.LastMsgKnown || s.LastMsg != 46*time.Millisecond || got.Stale(s) {
		t.Errorf("8469: got %+v want 46ms and not stale", s)
	}
	// Self carries no age, and that is not the same as never heard from.
	if s := byID["cc1b"]; !s.Self || s.LastMsgKnown || got.Stale(s) {
		t.Errorf("self: got %+v want Self and no age", s)
	}
	// The member that has gone.
	stale := byID["7ed9"]
	if !stale.LastMsgKnown {
		t.Fatalf("7ed9: no age parsed: %+v", stale)
	}
	if stale.LastMsg != 7965698*time.Millisecond {
		t.Errorf("7ed9: got %v want 7965698ms", stale.LastMsg)
	}
	if !got.Stale(stale) {
		t.Error("a member the leader has not heard from in 2.2 hours must read as stale")
	}
	// The leader's own replication figures say how far behind it actually is, which
	// does not depend on message timing at all.
	if behind, ok := got.Behind(stale); !ok || behind != 924823-921603 {
		t.Errorf("Behind: got %d (known=%v) want %d", behind, ok, 924823-921603)
	}

	if len(got.StaleServers()) != 1 {
		t.Errorf("StaleServers: got %d want 1", len(got.StaleServers()))
	}
	// A quorum of two out of three still works, so this is degraded, not down —
	// but it is emphatically not healthy.
	if got.Healthy() {
		t.Error("a cluster with an unresponsive member is not healthy")
	}
	if !got.HasLeader() {
		t.Error("there is a leader, so writes are working")
	}
}

func TestParseClusterStatus_Leader(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "sb-leader-healthy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsLeader() {
		t.Errorf("Role %q should read as leader", got.Role)
	}
	if !got.Healthy() {
		t.Errorf("a fully responsive cluster should be healthy: %+v", got)
	}
	if len(got.StaleServers()) != 0 {
		t.Errorf("no member should be stale: %+v", got.StaleServers())
	}
}

// An election in progress means writes are failing right now, which is a harder
// failure than a degraded quorum.
func TestParseClusterStatus_ElectionInProgress(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "election-in-progress.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != "candidate" {
		t.Errorf("Role: got %q want candidate", got.Role)
	}
	// "Leader: unknown" is no leader, not a leader named "unknown".
	if got.HasLeader() {
		t.Errorf("Leader: got %q want none", got.Leader)
	}
	if got.Healthy() {
		t.Error("no leader is not healthy")
	}
	if got.Uncommitted != 4 || got.Unapplied != 2 {
		t.Errorf("entries: got %d/%d want 4/2", got.Uncommitted, got.Unapplied)
	}
}

// An unrecognized line is skipped, since the format has no version to check and a
// new field must not cost the whole parse.
func TestParseClusterStatus_UnknownLinesAreSkipped(t *testing.T) {
	out := fixture(t, "sb-leader-healthy.txt") +
		"\nSome Future Field: 42\nanother unstructured line\n"
	got, err := ParseClusterStatus(out)
	if err != nil {
		t.Fatalf("an unknown line should not be fatal: %v", err)
	}
	if got.Role != "leader" {
		t.Errorf("known fields should still parse: got role=%q", got.Role)
	}
}

// Output with nothing recognizable is an error, since silently returning a zero
// status would look like a healthy cluster with no members.
func TestParseClusterStatus_UnrecognisableOutputIsAnError(t *testing.T) {
	for _, out := range []string{"", "command not found", "\n\n\n"} {
		if _, err := ParseClusterStatus(out); err == nil {
			t.Errorf("input %q should be an error", out)
		}
	}
}

func TestStaleThresholdBoundary(t *testing.T) {
	tests := []struct {
		name  string
		srv   Server
		stale bool
	}{
		{"fresh", Server{ID: "a", LastMsg: 40 * time.Millisecond, LastMsgKnown: true}, false},
		{"just inside", Server{ID: "a", LastMsg: StaleThreshold - time.Second, LastMsgKnown: true}, false},
		{"just outside", Server{ID: "a", LastMsg: StaleThreshold + time.Second, LastMsgKnown: true}, true},
		// Self has no age, and no age is not staleness.
		{"self", Server{ID: "a", Self: true}, false},
		{"no age reported", Server{ID: "a"}, false},
	}
	// Judged from a leader's status, since that is the only view staleness is read
	// from at all.
	leader := ClusterStatus{Role: "leader"}
	for _, tc := range tests {
		if got := leader.Stale(tc.srv); got != tc.stale {
			t.Errorf("%s: got stale=%v want %v", tc.name, got, tc.stale)
		}
	}
}

// --- settings -------------------------------------------------------------

func TestSettingsFrom(t *testing.T) {
	if got := SettingsFrom(nil); got != Defaults() {
		t.Errorf("got %+v want defaults", got)
	}
	got := SettingsFrom(map[string]any{"namespace": "ovn", "container": "db"})
	if got.Namespace != "ovn" || got.Container != "db" {
		t.Errorf("got %+v", got)
	}
}

// The namespace is left to discovery, since every hardcoded namespace in this
// codebase has been wrong on the first real cluster it met.
func TestDefaults_LeaveNamespaceUnset(t *testing.T) {
	if Defaults().Namespace != "" {
		t.Errorf("Namespace should be derived, got %q", Defaults().Namespace)
	}
	if Defaults().Container != "ovsdb" {
		t.Errorf("Container: got %q want ovsdb", Defaults().Container)
	}
}

// --- state ----------------------------------------------------------------

func stateFrom(t *testing.T, fixtures ...string) State {
	t.Helper()
	st := State{Tier: kube.TierFull, UpdatedAt: time.Now()}
	for _, f := range fixtures {
		parsed, err := ParseClusterStatus(fixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		st.Statuses = append(st.Statuses, parsed)
	}
	return st
}

func TestStateHealthy(t *testing.T) {
	if !stateFrom(t, "sb-leader-healthy.txt").Healthy() {
		t.Error("a healthy database should make the state healthy")
	}
	// One bad database is enough.
	mixed := stateFrom(t, "sb-leader-healthy.txt", "nb-leader-with-stale-member.txt")
	if mixed.Healthy() {
		t.Error("a stale member in either database means not healthy")
	}
	if (State{}).Healthy() {
		t.Error("no data is not healthy")
	}
}

// --- banner ---------------------------------------------------------------

func TestCells(t *testing.T) {
	s := store.New()

	// Healthy.
	s.Put(KeyState, stateFrom(t, "sb-leader-healthy.txt"))
	if got := (&Plugin{}).Cells(s)[0]; got.Status != tui.BannerOK {
		t.Errorf("healthy: got %v %q", got.Status, got.Detail)
	}

	// A stale member is a degraded quorum: a warning.
	s.Put(KeyState, stateFrom(t, "nb-leader-with-stale-member.txt"))
	cell := (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerWarn {
		t.Errorf("stale member: got %v want warn", cell.Status)
	}
	if !strings.Contains(cell.Detail, "stale") {
		t.Errorf("detail should name the problem: %q", cell.Detail)
	}

	// No leader is an outage: writes are failing.
	s.Put(KeyState, stateFrom(t, "election-in-progress.txt"))
	cell = (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerErr {
		t.Errorf("no leader: got %v want err", cell.Status)
	}
	if !strings.Contains(cell.Detail, "no leader") {
		t.Errorf("detail should say there is no leader: %q", cell.Detail)
	}
}

func TestCells_NoStateYet(t *testing.T) {
	if got := (&Plugin{}).Cells(store.New()); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

func TestCells_ReducedTier(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierInformer, TierReason: "no pods/exec"})
	if got := (&Plugin{}).Cells(s)[0]; got.Status != tui.BannerLoading {
		t.Errorf("got %v want loading (no detail available)", got.Status)
	}
}

// --- pane -----------------------------------------------------------------

func TestPane_ShowsRaftState(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "nb-leader-with-stale-member.txt", "sb-leader-healthy.txt"))
	body := stripANSI(newPane(s).Render(100, 12, false))

	for _, want := range []string{"nb", "sb", "leader", "317", "ovn-ovsdb-nb-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// The degraded quorum must be visible as a count and named below.
	if !strings.Contains(body, "2/3") {
		t.Errorf("expected 2 of 3 members responding:\n%s", body)
	}
	// The pod name, not the Raft ID: the ID identifies a member, but only the pod
	// name says which thing to go and look at.
	if !strings.Contains(body, "ovn-ovsdb-nb-2") {
		t.Errorf("the silent member should be named by pod:\n%s", body)
	}
	// And the duration is the point — hours, not milliseconds.
	if !strings.Contains(body, "2h") {
		t.Errorf("expected the silence expressed in hours:\n%s", body)
	}
}

func TestPane_ElectionIsCalledOut(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "election-in-progress.txt"))
	body := stripANSI(newPane(s).Render(110, 10, false))
	if !strings.Contains(body, "writes failing") {
		t.Errorf("an election should say writes are failing:\n%s", body)
	}
}

// A database that could not be read must not hide the other one.
func TestPane_PerDatabaseErrorIsIsolated(t *testing.T) {
	healthy := stateFrom(t, "sb-leader-healthy.txt")
	healthy.Statuses = append(healthy.Statuses, ClusterStatus{
		Database: "OVN_Northbound", Err: errFake{},
	})

	s := store.New()
	s.Put(KeyState, healthy)
	body := stripANSI(newPane(s).Render(110, 10, false))

	if !strings.Contains(body, "leader") {
		t.Errorf("the healthy database should still render:\n%s", body)
	}
	if !strings.Contains(body, "unreadable") {
		t.Errorf("the failed database should say so:\n%s", body)
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_States(t *testing.T) {
	if got := stripANSI(newPane(store.New()).Render(60, 8, false)); !strings.Contains(got, "loading") {
		t.Errorf("got %q want loading", got)
	}

	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierInformer, TierReason: "no pods/exec permission"})
	if got := stripANSI(newPane(s).Render(70, 8, false)); !strings.Contains(got, "pods/exec") {
		t.Errorf("a reduced tier should say why: %q", got)
	}
}

func TestPane_RespectsBounds(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "nb-leader-with-stale-member.txt", "sb-leader-healthy.txt"))
	p := newPane(s)

	for _, w := range []int{20, 46, 100, 220} {
		for _, h := range []int{1, 3, 8, 30} {
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

func TestHumanDuration(t *testing.T) {
	tests := map[time.Duration]string{
		45 * time.Second:           "45s",
		5 * time.Minute:            "5m",
		7965698 * time.Millisecond: "2.2h",
		50 * time.Hour:             "2.1d",
	}
	for in, want := range tests {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v): got %q want %q", in, got, want)
		}
	}
}

func TestDatabaseLabel(t *testing.T) {
	if got := databaseLabel("OVN_Northbound"); got != "nb" {
		t.Errorf("got %q want nb", got)
	}
	if got := databaseLabel("OVN_Southbound"); got != "sb" {
		t.Errorf("got %q want sb", got)
	}
	// An unknown name falls back to itself rather than to an empty cell.
	if got := databaseLabel("OVN_Something"); got != "OVN_Something" {
		t.Errorf("got %q", got)
	}
}

// --- member names ---------------------------------------------------------

// Every line but the Servers block refers to members by a four-hex-digit ID, which
// is useless for finding the pod to look at. The address in the Servers block is
// the only place the mapping appears.
func TestPodNameFrom(t *testing.T) {
	tests := map[string]string{
		"tcp:ovn-ovsdb-nb-2.ovn-ovsdb-nb.openstack.svc.cluster.local:6643": "ovn-ovsdb-nb-2",
		"ssl:ovn-ovsdb-sb-0.ovn-ovsdb-sb.openstack.svc.cluster.local:6644": "ovn-ovsdb-sb-0",
		// A bare hostname is already the answer.
		"tcp:ovn-ovsdb-nb-1:6643": "ovn-ovsdb-nb-1",
		// An IP is returned as the IP: half a name would be worse than none.
		"tcp:10.0.0.5:6643": "10.0.0.5",
		// A bracketed IPv6 literal must not have its colons mistaken for a port.
		"tcp:[fd00::1]:6643": "fd00::1",
		"":                   "",
	}
	for in, want := range tests {
		if got := PodNameFrom(in); got != want {
			t.Errorf("PodNameFrom(%q): got %q want %q", in, got, want)
		}
	}
}

func TestParseClusterStatus_MemberNames(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Server{}
	for _, s := range got.Servers {
		byID[s.ID] = s
	}
	want := map[string]string{
		"cc1b": "ovn-ovsdb-nb-1",
		"8469": "ovn-ovsdb-nb-0",
		"7ed9": "ovn-ovsdb-nb-2",
	}
	for id, name := range want {
		if got := byID[id].Name; got != name {
			t.Errorf("%s: got name %q want %q", id, got, name)
		}
		if byID[id].DisplayName() != name {
			t.Errorf("%s: DisplayName should prefer the pod name", id)
		}
	}
}

// A member with no address falls back to its ID rather than rendering an empty
// cell.
func TestServerDisplayName_FallsBackToID(t *testing.T) {
	if got := (Server{ID: "abcd"}).DisplayName(); got != "abcd" {
		t.Errorf("got %q want abcd", got)
	}
}

// The leader is reported by ID, so it has to be resolved against the Servers
// block.
func TestLeaderName_ResolvesFromID(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Leader != "cc1b" {
		t.Fatalf("Leader: got %q want the raw id cc1b", got.Leader)
	}
	if name := got.LeaderName(); name != "ovn-ovsdb-nb-1" {
		t.Errorf("LeaderName: got %q want ovn-ovsdb-nb-1", name)
	}
}

// "Leader: self" is the literal the status uses when this member leads, so it
// resolves to the queried member's own name.
func TestLeaderName_ResolvesSelf(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "sb-leader-healthy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if name := got.LeaderName(); name != "ovn-ovsdb-sb-1" {
		t.Errorf("LeaderName: got %q want ovn-ovsdb-sb-1", name)
	}
}

// An ID with no matching member falls back to the ID: a member can be named as
// leader before its own entry appears.
func TestLeaderName_UnknownIDFallsBack(t *testing.T) {
	st := ClusterStatus{Leader: "ffff", Servers: []Server{{ID: "aaaa", Name: "pod-a"}}}
	if got := st.LeaderName(); got != "ffff" {
		t.Errorf("got %q want the raw id", got)
	}
}

func TestLeaderName_NoLeader(t *testing.T) {
	if got := (ClusterStatus{}).LeaderName(); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- compact summary ------------------------------------------------------

// The compact form is what a narrow pane and a diagnostic line show, and it must
// name the lagging pod and its lag rather than only counting members.
func TestSummary(t *testing.T) {
	state := stateFrom(t, "nb-leader-with-stale-member.txt", "sb-leader-healthy.txt")
	got := Summary(state)
	if len(got) != 2 {
		t.Fatalf("got %d lines want 2: %v", len(got), got)
	}

	if got[0] != "nb: leader=ovn-ovsdb-nb-1 term=317 lag: ovn-ovsdb-nb-2 2.2h" {
		t.Errorf("northbound line: got %q", got[0])
	}
	if got[1] != "sb: leader=ovn-ovsdb-sb-1 term=42 members 3/3" {
		t.Errorf("southbound line: got %q", got[1])
	}
}

func TestSummary_UnreadableDatabase(t *testing.T) {
	state := State{Statuses: []ClusterStatus{{Database: "OVN_Northbound", Err: errFake{}}}}
	got := Summary(state)
	if len(got) != 1 || !strings.Contains(got[0], "unreadable") {
		t.Errorf("got %v", got)
	}
}

// A narrow pane shows the summary rather than a mangled table.
func TestPane_NarrowShowsSummary(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "nb-leader-with-stale-member.txt", "sb-leader-healthy.txt"))
	body := stripANSI(newPane(s).Render(60, 8, false))

	if strings.Contains(body, "TERM") {
		t.Errorf("a narrow pane should not draw the table:\n%s", body)
	}
	if !strings.Contains(body, "leader=ovn-ovsdb-nb-1") {
		t.Errorf("the summary should name the leader:\n%s", body)
	}
	if !strings.Contains(body, "lag:") {
		t.Errorf("the summary should report the lag:\n%s", body)
	}
}

// --- whose view is "last msg"? --------------------------------------------

// The false alarm this replaced, and the reason the plugin now goes looking for
// the leader.
//
// This fixture is a real follower's status from a healthy cluster. It reports its
// peer as last heard from 7,965,698 ms — 2.2 hours — ago, and that is *normal*: in
// Raft the leader heartbeats its followers and they answer the leader, so two
// followers exchange nothing between elections. The same cluster's leader, read
// seconds later, had heard from both followers 46ms earlier with their logs fully
// replicated.
func TestFollowerViewReportsNoStaleness(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsLeader() {
		t.Fatalf("fixture should be a follower, got role %q", got.Role)
	}

	// The number is still parsed — it is in the output, and hiding it would be its
	// own kind of lie.
	var peer Server
	for _, s := range got.Servers {
		if s.ID == "7ed9" {
			peer = s
		}
	}
	if !peer.LastMsgKnown || peer.LastMsg != 7965698*time.Millisecond {
		t.Fatalf("peer age not parsed: %+v", peer)
	}

	// But it is not a health signal from this view.
	if got.Stale(peer) {
		t.Error("a follower's view of another follower must not read as staleness")
	}
	if n := len(got.StaleServers()); n != 0 {
		t.Errorf("StaleServers from a follower: got %d, want none", n)
	}
	if got.MemberViewTrusted() {
		t.Error("a follower cannot speak to the other members' health")
	}
	// Replication figures are the leader's to report, so there are none here.
	if _, ok := got.Behind(peer); ok {
		t.Error("a follower has no match_index for its peers")
	}
}

// The same silence, read from the leader, is the real thing.
func TestLeaderViewReportsStaleness(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-leader-with-stale-member.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.MemberViewTrusted() {
		t.Fatal("a leader's view is the one that counts")
	}
	stale := got.StaleServers()
	if len(stale) != 1 || stale[0].Name != "ovn-ovsdb-nb-2" {
		t.Fatalf("StaleServers: got %+v want just ovn-ovsdb-nb-2", stale)
	}
}

// A healthy leader — the live payload this was fixed against — reports nothing
// wrong at all. Getting this wrong in the other direction would be worse than the
// original bug: silence where there is a real problem.
func TestHealthyLeaderIsClean(t *testing.T) {
	got, err := ParseClusterStatus(fixture(t, "nb-leader-healthy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsLeader() || !got.HasLeader() {
		t.Fatalf("expected a leading member: role=%q leader=%q", got.Role, got.Leader)
	}
	if len(got.StaleServers()) != 0 {
		t.Errorf("healthy cluster reported stale members: %+v", got.StaleServers())
	}
	if !got.Healthy() {
		t.Error("this cluster is healthy")
	}
	// One entry behind the leader's log end is the steady state, not a problem —
	// which is why Behind carries no threshold of its own.
	for _, s := range got.Servers {
		if s.Self {
			continue
		}
		behind, ok := got.Behind(s)
		if !ok {
			t.Errorf("%s: leader should report match_index", s.DisplayName())
		}
		if behind > 1 {
			t.Errorf("%s: behind %d, expected at most 1 in steady state", s.DisplayName(), behind)
		}
	}
}

// A follower's status must not claim the members are fine either. The mirror image
// of a false alarm is a blind spot reported as health.
func TestSummarySaysWhenMembersWereNotChecked(t *testing.T) {
	st, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st.Pod = "ovn-ovsdb-nb-0"
	lines := Summary(State{Tier: kube.TierFull, Statuses: []ClusterStatus{st}})

	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	if strings.Contains(lines[0], "3/3") {
		t.Errorf("a follower's view must not claim all members are fine: %q", lines[0])
	}
	for _, want := range []string{"unchecked", "ovn-ovsdb-nb-0"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("summary should say whose view it is (%q): %q", want, lines[0])
		}
	}
}

func TestPane_FollowerViewSaysMembersNotChecked(t *testing.T) {
	st, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st.Pod = "ovn-ovsdb-nb-0"
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Statuses: []ClusterStatus{st}, UpdatedAt: time.Now()})

	body := stripANSI(newPane(s).Render(110, 10, false))
	if !strings.Contains(body, "not checked") {
		t.Errorf("pane should say the members were not checked:\n%s", body)
	}
	if strings.Contains(body, "not responding") {
		t.Errorf("pane must not claim a member is unresponsive from a follower's view:\n%s", body)
	}
}

// The banner grades a healthy cluster green whichever member answered, so a
// follower's view cannot raise a false alarm there either.
func TestCells_FollowerViewIsNotAnAlarm(t *testing.T) {
	st, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Statuses: []ClusterStatus{st}, UpdatedAt: time.Now()})

	if got := (&Plugin{}).Cells(s)[0]; got.Status == tui.BannerErr {
		t.Errorf("a healthy cluster read from a follower must not be red: %+v", got)
	}
}

// Green, but with the gap named: nothing is known to be wrong, and the members
// could not be checked. Both halves are true and the second must be visible.
func TestCells_UncheckedMembersAreNamed(t *testing.T) {
	st, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Statuses: []ClusterStatus{st}, UpdatedAt: time.Now()})

	got := (&Plugin{}).Cells(s)[0]
	if got.Status != tui.BannerOK {
		t.Errorf("status = %v, want OK — nothing is known to be wrong", got.Status)
	}
	if !strings.Contains(got.Detail, "unchecked") {
		t.Errorf("detail should name the gap, got %q", got.Detail)
	}
}

// A fraction asserts the numerator was checked, so a follower's row shows a count
// instead. "3/3" next to "members not checked" would contradict itself.
func TestPane_FollowerRowDoesNotClaimAFraction(t *testing.T) {
	st, err := ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st.Pod = "ovn-ovsdb-nb-0"
	s := store.New()
	s.Put(KeyState, State{Tier: kube.TierFull, Statuses: []ClusterStatus{st}, UpdatedAt: time.Now()})

	body := stripANSI(newPane(s).Render(110, 10, false))
	if strings.Contains(body, "3/3") {
		t.Errorf("a follower's row must not claim 3 of 3 checked:\n%s", body)
	}
}
