package pane

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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

func populatedState() State {
	return State{
		Pools: []Pool{
			{Name: "primary", Addresses: []string{"10.0.0.10-10.0.0.50"}, AutoAssign: true,
				Advertised: []string{"L2"}, Assigned: 3},
			{Name: "manual-only", Addresses: []string{"10.1.0.0/24"}, AutoAssign: false,
				Advertised: []string{"BGP"}},
			{Name: "orphan", Addresses: []string{"10.2.0.1"}, AutoAssign: true},
		},
		Services:       []Service{{Name: "ok", ExternalIP: "10.0.0.11"}, {Name: "waiting"}},
		SpeakerReady:   3,
		SpeakerDesired: 3,
		UpdatedAt:      time.Now(),
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

func TestPane_Render(t *testing.T) {
	s := store.New()
	s.Put(KeyState, populatedState())
	body := stripANSI(newPane(s).Render(90, 12, false))

	for _, want := range []string{"primary", "10.0.0.10-10.0.0.50", "L2", "speaker 3/3"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "none") {
		t.Errorf("an unadvertised pool should read as 'none':\n%s", body)
	}
	if !strings.Contains(body, "manual") {
		t.Errorf("a manual-only pool should be marked:\n%s", body)
	}
	if !strings.Contains(body, "pending") {
		t.Errorf("pending services should be reported:\n%s", body)
	}
}

func TestPane_RespectsBounds(t *testing.T) {
	s := store.New()
	s.Put(KeyState, populatedState())
	p := newPane(s)

	for _, w := range []int{20, 40, 90, 200} {
		for _, h := range []int{1, 2, 5, 20} {
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

func TestPane_States(t *testing.T) {
	if got := stripANSI(newPane(store.New()).Render(60, 8, false)); !strings.Contains(got, "loading") {
		t.Errorf("got %q want a loading state", got)
	}

	s := store.New()
	s.Put(KeyState, State{UpdatedAt: time.Now()})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "no address pools") {
		t.Errorf("got %q want an empty state", got)
	}

	s = store.New()
	s.Put(KeyState, State{Err: errFake{}, UpdatedAt: time.Now()})
	if got := stripANSI(newPane(s).Render(60, 8, false)); !strings.Contains(got, "boom") {
		t.Errorf("got %q want the error surfaced", got)
	}
}

func TestPane_PartialFailureStillRendersPools(t *testing.T) {
	s := store.New()
	state := populatedState()
	state.Err = errFake{}
	s.Put(KeyState, state)

	body := stripANSI(newPane(s).Render(90, 12, false))
	if !strings.Contains(body, "primary") {
		t.Errorf("pools should survive a partial failure:\n%s", body)
	}
}

func TestPane_Identity(t *testing.T) {
	p := newPane(store.New())
	if p.ID() != "metallb" {
		t.Errorf("ID: got %q", p.ID())
	}
	if p.Priority() != tui.P1Important {
		t.Errorf("Priority: got %v", p.Priority())
	}
}

func TestUsageCell(t *testing.T) {
	tests := []struct {
		name string
		pool Pool
		want string
	}{
		{"published", Pool{Assigned: 9, Available: 79, Usage: UsageStatus}, "9/88"},
		{"empty pool", Pool{Assigned: 0, Available: 88, Usage: UsageStatus}, "0/88"},
		{"full pool", Pool{Assigned: 88, Available: 0, Usage: UsageStatus}, "88/88"},
		{"counted from annotations", Pool{Assigned: 3, Usage: UsageAnnotations}, "3"},
		{"unmeasurable", Pool{Usage: UsageUnknown}, "?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := usage(tc.pool); got != tc.want {
				t.Errorf("usage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsageCell_ColorsAPoolRunningOut(t *testing.T) {
	plain := func(p Pool) string { _, st := usage(p); return st.Render("x") }

	roomy := plain(Pool{Assigned: 10, Available: 90, Usage: UsageStatus})
	nearly := plain(Pool{Assigned: 95, Available: 5, Usage: UsageStatus})
	full := plain(Pool{Assigned: 100, Available: 0, Usage: UsageStatus})

	if roomy == nearly {
		t.Error("a pool with a tenth left looks like one that is barely used")
	}
	if nearly == full {
		t.Error("an exhausted pool looks like one that is merely close")
	}
}

func TestPane_ShowsUsageOnAnAutoAssignCluster(t *testing.T) {
	pools := []Pool{
		{Namespace: "kube-system", Name: "default",
			Addresses: []string{"10.4.192.12-10.4.192.99"}, AutoAssign: true,
			Advertised: []string{"L2"}, Assigned: 9, Available: 79, Usage: UsageStatus},
		{Namespace: "kube-system", Name: "rook-rgw-replication",
			Addresses: []string{"10.252.4.20-10.252.4.20"}, AutoAssign: true,
			Advertised: []string{"L2"}, Assigned: 0, Available: 1, Usage: UsageStatus},
	}
	services := make([]Service, 0, 9)
	for i := range 9 {
		services = append(services, Service{
			Namespace: "mgd-rabbitmq", Name: fmt.Sprintf("svc-%d", i),
			ExternalIP: fmt.Sprintf("10.4.192.%d", 15+i),
			Pool:       "default",
		})
	}

	s := store.New()
	s.Put(KeyState, State{
		Pools: pools, Services: services, Namespace: "kube-system",
		SpeakerReady: 3, SpeakerDesired: 3, UpdatedAt: time.Now(),
	})
	body := stripANSI(newPane(s).Render(100, 12, false))

	if !strings.Contains(body, "9/88") {
		t.Errorf("the default pool does not report its usage:\n%s", body)
	}
	if !strings.Contains(body, "0/1") {
		t.Errorf("the unused pool does not report its capacity:\n%s", body)
	}
	if strings.Contains(body, "Service(s) pending") {
		t.Errorf("nothing is pending, but the summary says otherwise:\n%s", body)
	}
}
