package pane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/internal/plugin/ovn"
	"github.com/runlevel-six/binnacle/pkg/store"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
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

func stateFrom(t *testing.T, fixtures ...string) State {
	t.Helper()
	st := State{Tier: kube.TierFull, UpdatedAt: time.Now()}
	for _, f := range fixtures {
		parsed, err := ovn.ParseClusterStatus(fixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		st.Statuses = append(st.Statuses, parsed)
	}
	return st
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_ShowsRaftState(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "nb-leader-with-stale-member.txt", "sb-leader-healthy.txt"))
	body := stripANSI(newPane(s).Render(100, 12, false))

	for _, want := range []string{"nb", "sb", "leader", "317", "ovn-ovsdb-nb-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "2/3") {
		t.Errorf("expected 2 of 3 members responding:\n%s", body)
	}
	if !strings.Contains(body, "ovn-ovsdb-nb-2") {
		t.Errorf("the silent member should be named by pod:\n%s", body)
	}
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
	if got := databaseLabel("OVN_Something"); got != "OVN_Something" {
		t.Errorf("got %q", got)
	}
}

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

func TestSummarySaysWhenMembersWereNotChecked(t *testing.T) {
	st, err := ovn.ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
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
	st, err := ovn.ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
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

func TestPane_FollowerRowDoesNotClaimAFraction(t *testing.T) {
	st, err := ovn.ParseClusterStatus(fixture(t, "nb-follower-quiet-peer.txt"))
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
