package pane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/internal/plugin/ceph"
	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/store"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
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

func stateFrom(t *testing.T, name string) State {
	t.Helper()
	status, err := ceph.ParseStatus(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return State{Tier: kube.TierFull, Status: status, Pod: "rook-ceph-tools-abc", UpdatedAt: time.Now()}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_HealthyCluster(t *testing.T) {
	s := store.New()
	s.Put(KeyState, stateFrom(t, "reef-healthy-summary-mgrmap.json"))
	body := stripANSI(newPane(s).Render(80, 12, false))

	for _, want := range []string{"HEALTH_OK", "3/3 in quorum", "36/36 up", "active+clean"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "12%") {
		t.Errorf("expected raw usage percentage:\n%s", body)
	}
	if !strings.Contains(body, "stored") {
		t.Errorf("expected stored data alongside raw usage:\n%s", body)
	}
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
	if !strings.Contains(body, "muted") {
		t.Errorf("expected the muted count:\n%s", body)
	}
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
	for _, expect := range []string{"HEALTH_WARN", "OSD_DOWN", "398/408", "10% used"} {
		if !strings.Contains(body, expect) {
			t.Errorf("block should report %q:\n%s", expect, body)
		}
	}
	if len(block.Lines) > 3 {
		t.Errorf("block has %d lines, want at most 3", len(block.Lines))
	}
}

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

func TestSummary_SilentWithoutData(t *testing.T) {
	if _, want := summaryBlock(store.New()); want {
		t.Error("an empty store should contribute no block")
	}
}
