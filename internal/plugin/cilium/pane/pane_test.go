package pane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/internal/plugin/cilium"
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

func fullState(t *testing.T) State {
	t.Helper()
	status, err := cilium.ParseStatus(fixture(t, "1.16-summary-ipam.json"))
	if err != nil {
		t.Fatal(err)
	}
	return State{
		Tier: kube.TierFull, AgentsReady: 6, AgentsDesired: 6,
		Status: status, Pod: "cilium-abc12", UpdatedAt: time.Now(),
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_FullTier(t *testing.T) {
	s := store.New()
	s.Put(KeyState, fullState(t))
	body := stripANSI(newPane(s).Render(70, 14, false))

	for _, want := range []string{"6/6 ready", "1.16.5", "kube-proxy", "42/256", "16%"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "cilium-abc12") {
		t.Errorf("IPAM should be labeled with its pod:\n%s", body)
	}
}

func TestPane_InformerTierExplainsItself(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{
		Tier:          kube.TierInformer,
		AgentsReady:   6,
		AgentsDesired: 6,
		TierReason:    "no pods/exec permission on kube-system",
		UpdatedAt:     time.Now(),
	})
	body := stripANSI(newPane(s).Render(70, 10, false))

	if !strings.Contains(body, "6/6 ready") {
		t.Errorf("agent readiness should survive the reduced tier:\n%s", body)
	}
	if !strings.Contains(body, "detail unavailable") {
		t.Errorf("a reduced tier should say so:\n%s", body)
	}
	if !strings.Contains(body, "pods/exec") {
		t.Errorf("a reduced tier should say why:\n%s", body)
	}
}

func TestPane_UnknownIPAMShowsNoPercentage(t *testing.T) {
	s := store.New()
	state := fullState(t)
	state.Status.IPAM = IPAM{Used: 7}
	s.Put(KeyState, state)

	body := stripANSI(newPane(s).Render(70, 14, false))
	if !strings.Contains(body, "7 allocated") {
		t.Errorf("expected a bare allocation count:\n%s", body)
	}
	if strings.Contains(body, "100%") {
		t.Errorf("unknown exhaustion must not render as full:\n%s", body)
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
	s := store.New()
	s.Put(KeyState, fullState(t))
	p := newPane(s)

	for _, w := range []int{20, 40, 70, 200} {
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

func TestPane_NamesUnreadableSections(t *testing.T) {
	s := store.New()
	state := fullState(t)
	state.Status.Unreadable = []string{"ipam"}
	s.Put(KeyState, state)

	body := stripANSI(newPane(s).Render(70, 16, false))
	if !strings.Contains(body, "unreadable") || !strings.Contains(body, "ipam") {
		t.Errorf("expected the unreadable section named:\n%s", body)
	}
}

func TestPane_KubeProxyReplacementIsNotMistakenForKubeProxy(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{
		Tier: kube.TierFull, AgentsReady: 6, AgentsDesired: 6,
		Status: Status{Version: "1.19.6", State: "Ok", KubeProxyReplacement: "True"},
	})

	body := stripANSI(newPane(s).Render(70, 14, false))
	if !strings.Contains(body, "replaced by Cilium") {
		t.Errorf("pane should say kube-proxy was replaced, got:\n%s", body)
	}
	if strings.Contains(body, "kube-proxy     True") {
		t.Errorf("pane still renders the raw mode:\n%s", body)
	}
}
