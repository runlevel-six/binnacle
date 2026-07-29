package openstack

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
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

// realAgents mirrors the shape a production cloud reports: compute nodes in the
// nova zone with FQDN hosts, and control services in the internal zone whose hosts
// are pod names.
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

// --- up versus enabled ----------------------------------------------------

// The distinction the whole plugin turns on. A *disabled* agent is intentional —
// that is how a compute node is drained before maintenance — while a *down* agent
// is a failure. Conflating them would cry wolf through every planned maintenance
// window, which is exactly when this dashboard is being watched.
func TestAgentHealthy_DisabledIsNotBroken(t *testing.T) {
	tests := []struct {
		name    string
		agent   Agent
		healthy bool
	}{
		{"up and enabled", Agent{Up: true, Enabled: true}, true},
		// Drained for maintenance: deliberate, so not a fault.
		{"down and disabled", Agent{Up: false, Enabled: false}, true},
		{"up but disabled", Agent{Up: true, Enabled: false}, true},
		// The only combination that needs attention.
		{"down and enabled", Agent{Up: false, Enabled: true}, false},
	}
	for _, tc := range tests {
		if got := tc.agent.Healthy(); got != tc.healthy {
			t.Errorf("%s: got healthy=%v want %v", tc.name, got, tc.healthy)
		}
	}
}

func TestSummarise(t *testing.T) {
	got := Summarize(realAgents())
	if len(got) != 3 {
		t.Fatalf("services: got %d want 3", len(got))
	}

	byName := map[string]ServiceSummary{}
	for _, s := range got {
		byName[s.Service] = s
	}
	compute := byName[ServiceCompute]
	if compute.Total != 4 || compute.Up != 4 {
		t.Errorf("compute: got %+v want 4/4", compute)
	}
	if !compute.Healthy() {
		t.Error("all agents up should be healthy")
	}
}

// A drained compute node reduces the up count without making the service
// unhealthy, and is reported separately so it stays visible.
func TestSummarise_DrainedNodeIsNotUnhealthy(t *testing.T) {
	agents := append(realAgents(), Agent{
		Service: ServiceCompute, Binary: "nova-compute",
		Host: "compute-node-3.site-a.example.com", Zone: "nova",
		Up: false, Enabled: false,
	})

	compute := Summarize(agents)[0]
	if compute.Service != ServiceCompute {
		t.Fatalf("expected compute first, got %q", compute.Service)
	}
	if compute.Total != 5 || compute.Up != 4 {
		t.Errorf("got %d/%d up want 4/5", compute.Up, compute.Total)
	}
	if compute.Disabled != 1 {
		t.Errorf("Disabled: got %d want 1", compute.Disabled)
	}
	if !compute.Healthy() {
		t.Error("a drained node must not make the service unhealthy")
	}
	if len(compute.DownBinaries) != 0 {
		t.Errorf("a disabled agent is not 'down': got %v", compute.DownBinaries)
	}
}

// A down-and-enabled agent is named by binary, since "3 down" does not say whether
// it is every compute node or one scheduler.
func TestSummarise_DownAgentsAreNamedByBinary(t *testing.T) {
	agents := append(realAgents(),
		Agent{Service: ServiceCompute, Binary: "nova-compute", Host: "broken-1", Up: false, Enabled: true},
		Agent{Service: ServiceCompute, Binary: "nova-compute", Host: "broken-2", Up: false, Enabled: true},
		Agent{Service: ServiceCompute, Binary: "nova-conductor", Host: "broken-3", Up: false, Enabled: true},
	)

	compute := Summarize(agents)[0]
	if compute.Healthy() {
		t.Error("down enabled agents should make the service unhealthy")
	}
	// Deduplicated and sorted: two broken nova-computes are one binary.
	if strings.Join(compute.DownBinaries, ",") != "nova-compute,nova-conductor" {
		t.Errorf("DownBinaries: got %v", compute.DownBinaries)
	}
}

func TestSummarise_Empty(t *testing.T) {
	if got := Summarize(nil); len(got) != 0 {
		t.Errorf("got %v want empty", got)
	}
}

// --- state ----------------------------------------------------------------

func stateFrom(agents []Agent) State {
	return State{
		Cloud: "my-cloud", Region: "dev-1",
		Agents: agents, Services: Summarize(agents), UpdatedAt: time.Now(),
	}
}

func TestState_DownAndDisabledAreSeparate(t *testing.T) {
	agents := append(realAgents(),
		Agent{Service: ServiceCompute, Binary: "nova-compute", Host: "drained", Up: false, Enabled: false},
		Agent{Service: ServiceCompute, Binary: "nova-compute", Host: "broken", Up: false, Enabled: true},
	)
	st := stateFrom(agents)

	down := st.DownAgents()
	if len(down) != 1 || down[0].Host != "broken" {
		t.Errorf("DownAgents: got %+v want just the broken one", down)
	}
	disabled := st.DisabledAgents()
	if len(disabled) != 1 || disabled[0].Host != "drained" {
		t.Errorf("DisabledAgents: got %+v want just the drained one", disabled)
	}
}

// --- settings -------------------------------------------------------------

// There is no default cloud name: it is chosen by whoever wrote clouds.yaml, and
// guessing would produce an authentication failure rather than an absent plugin.
func TestSettingsFrom(t *testing.T) {
	if got := SettingsFrom(nil); got.Cloud != "" {
		t.Errorf("got %q want no default cloud", got.Cloud)
	}
	if got := SettingsFrom(map[string]any{"cloud": "my-cloud"}); got.Cloud != "my-cloud" {
		t.Errorf("got %q want my-cloud", got.Cloud)
	}
}

// Most users of this tool do not run OpenStack, so an unconfigured cloud means the
// plugin is not wanted — not that something failed.
func TestDetect_UnconfiguredIsAbsentNotAnError(t *testing.T) {
	active, err := New(Settings{}).Detect(t.Context())
	if err != nil {
		t.Errorf("an unconfigured cloud should not be an error: %v", err)
	}
	if active {
		t.Error("an unconfigured cloud should not activate the plugin")
	}
}

// A *named* cloud that cannot be resolved is reported, since that is a mistake the
// user wants to know about rather than a silently missing pane.
func TestDetect_NamedButMissingCloudIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/clouds.yaml", []byte("clouds:\n  other: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OS_CLIENT_CONFIG_FILE", dir+"/clouds.yaml")

	if _, err := New(Settings{Cloud: "no-such-cloud"}).Detect(t.Context()); err == nil {
		t.Error("a named cloud that does not resolve should be an error")
	}
}

// gophercloud's loader panics on a malformed entry rather than returning an error,
// and the usual cause is the flat legacy layout that requires a nested `auth:`
// block here. Letting that escape would take the whole dashboard down over one
// misconfigured file.
func TestParseClouds_MalformedEntryBecomesAnError(t *testing.T) {
	dir := t.TempDir()
	// The flat layout: auth fields at the top level with no nested auth: block.
	flat := "clouds:\n  flat:\n    auth_url: https://example.test/v3\n    username: admin\n"
	if err := os.WriteFile(dir+"/clouds.yaml", []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OS_CLIENT_CONFIG_FILE", dir+"/clouds.yaml")

	// Whether this panics or errors is gophercloud's business; what matters is
	// that it never panics out of here.
	_, _, _, err := parseClouds("flat")
	if err == nil {
		t.Skip("this gophercloud release accepts the flat layout; nothing to convert")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Errorf("a panic should be converted, not reported verbatim: %v", err)
	}
}

func TestParseClouds_DoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/clouds.yaml", []byte("not: [valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OS_CLIENT_CONFIG_FILE", dir+"/clouds.yaml")

	// The assertion is simply that this returns rather than panicking.
	if _, _, _, err := parseClouds("anything"); err == nil {
		t.Error("expected an error for an unparseable file")
	}
}

// --- banner ---------------------------------------------------------------

func TestCells(t *testing.T) {
	s := store.New()

	s.Put(KeyState, stateFrom(realAgents()))
	if got := (&Plugin{}).Cells(s)[0]; got.Status != tui.BannerOK {
		t.Errorf("healthy: got %v %q", got.Status, got.Detail)
	}

	// A drained node is deliberate: amber, not red — but still shown, since a node
	// left disabled after maintenance is a common oversight.
	drained := append(realAgents(), Agent{Service: ServiceCompute, Binary: "nova-compute",
		Host: "drained", Up: false, Enabled: false})
	s.Put(KeyState, stateFrom(drained))
	cell := (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerWarn {
		t.Errorf("drained: got %v want warn", cell.Status)
	}
	if !strings.Contains(cell.Detail, "disabled") {
		t.Errorf("detail should say disabled: %q", cell.Detail)
	}

	// A down-and-enabled agent is a failure.
	broken := append(realAgents(), Agent{Service: ServiceCompute, Binary: "nova-compute",
		Host: "broken", Up: false, Enabled: true})
	s.Put(KeyState, stateFrom(broken))
	cell = (&Plugin{}).Cells(s)[0]
	if cell.Status != tui.BannerErr {
		t.Errorf("broken: got %v want err", cell.Status)
	}
	if !strings.Contains(cell.Detail, "down") {
		t.Errorf("detail should say down: %q", cell.Detail)
	}
}

func TestCells_Unreachable(t *testing.T) {
	s := store.New()
	s.Put(KeyState, State{Cloud: "my-cloud", Err: errFake{}, UpdatedAt: time.Now()})
	if got := (&Plugin{}).Cells(s)[0]; got.Status != tui.BannerErr {
		t.Errorf("got %v want err", got.Status)
	}
}

func TestCells_NoStateYet(t *testing.T) {
	if got := (&Plugin{}).Cells(store.New()); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

// --- pane -----------------------------------------------------------------

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
	// FQDN domains are identical on every row, so they are trimmed.
	if strings.Contains(body, "site-a.example.com") {
		t.Errorf("host domains should be trimmed:\n%s", body)
	}
}

// One unavailable service must not hide the others.
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
