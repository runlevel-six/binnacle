package cilium

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

// fixture reads captured `cilium status -o json` output.
//
// Fixtures rather than hand-built JSON because the schema is unversioned and this
// is the only way to notice a release changing shape: a fixture is a record of
// what a real Cilium actually emitted.
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

// --- parsing across releases ---------------------------------------------

// Every fixture must parse. This is the test that catches a release changing
// shape, which the IPAM block has done at least four times.
func TestParseStatus_AllFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures — the parser would be untested")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			got, err := ParseStatus(fixture(t, e.Name()))
			if err != nil {
				t.Fatalf("does not parse: %v", err)
			}
			if got.Version == "" {
				t.Error("version should be populated in every fixture")
			}
		})
	}
}

func TestParseStatus_SummaryIPAM(t *testing.T) {
	got, err := ParseStatus(fixture(t, "1.16-summary-ipam.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Build metadata is stripped: it is noise in a status cell.
	if got.Version != "1.16.5" {
		t.Errorf("Version: got %q want 1.16.5", got.Version)
	}
	if got.State != "Ok" || got.KubeProxyReplacement != "True" {
		t.Errorf("got %+v", got)
	}
	if got.IPAM.Used != 42 || got.IPAM.Available != 214 {
		t.Errorf("IPAM: got %+v want 42/214", got.IPAM)
	}
	if got.IPAM.Total() != 256 {
		t.Errorf("Total: got %d want 256", got.IPAM.Total())
	}
	if got.Controllers.Total != 2 || got.Controllers.Failing != 0 {
		t.Errorf("Controllers: got %+v", got.Controllers)
	}
	if !got.Hubble.Enabled || got.Hubble.SeenFlows != 987654321 {
		t.Errorf("Hubble: got %+v", got.Hubble)
	}
}

func TestParseStatus_PerPoolIPAMIsSummed(t *testing.T) {
	got, err := ParseStatus(fixture(t, "1.15-per-pool-ipam.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.IPAM.Used != 15 || got.IPAM.Available != 120 {
		t.Errorf("IPAM: got %+v want 15/120 (summed across pools)", got.IPAM)
	}
	if got.EncryptionMode != "WireGuard" {
		t.Errorf("EncryptionMode: got %q", got.EncryptionMode)
	}
	// A controller with consecutive failures counts as failing.
	if got.Controllers.Failing != 1 {
		t.Errorf("Controllers.Failing: got %d want 1", got.Controllers.Failing)
	}
	if len(got.ClusterMesh.Peers) != 2 || got.ClusterMesh.GlobalServices != 7 {
		t.Errorf("ClusterMesh: got %+v", got.ClusterMesh)
	}
}

// The oldest shape lists allocated addresses with no remaining count, so Available
// is zero because it is *unknown*. Reporting 100% used would be a false alarm on
// exactly the metric an operator reacts to.
func TestParseStatus_AllocationListHasUnknownExhaustion(t *testing.T) {
	got, err := ParseStatus(fixture(t, "1.14-allocation-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.IPAM.Used != 3 {
		t.Errorf("Used: got %d want 3", got.IPAM.Used)
	}
	if got.IPAM.ExhaustionKnown() {
		t.Error("exhaustion is not knowable from an allocation list")
	}
	if got.IPAM.Percent() != -1 {
		t.Errorf("Percent: got %d want -1 (unknown)", got.IPAM.Percent())
	}
	// Hubble explicitly disabled is not the same as absent.
	if got.Hubble.Enabled {
		t.Error("Hubble should read as disabled")
	}
}

func TestParseStatus_BareArrayIPAM(t *testing.T) {
	got, err := ParseStatus(fixture(t, "bare-array-ipam.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.IPAM.Used != 3 || got.IPAM.Available != 7 {
		t.Errorf("IPAM: got %+v want 3/7", got.IPAM)
	}
}

// A document with almost nothing in it must still parse. Losing one cell beats
// losing the pane, and this is the shape an unfamiliar release most resembles.
func TestParseStatus_MinimalDocument(t *testing.T) {
	got, err := ParseStatus(fixture(t, "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.16.0" {
		t.Errorf("Version: got %q", got.Version)
	}
	if got.IPAM.Total() != 0 || got.Controllers.Total != 0 {
		t.Errorf("absent blocks should be zero, got %+v", got)
	}
	// An absent Hubble block is not "disabled" — we simply do not know.
	if got.Hubble.Enabled || got.Hubble.State != "" {
		t.Errorf("Hubble: got %+v want zero", got.Hubble)
	}
}

func TestParseStatus_MalformedJSON(t *testing.T) {
	if _, err := ParseStatus([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseStatus_EmptyIPAMBlock(t *testing.T) {
	got, err := ParseStatus([]byte(`{"cilium":{"version":"1.16.0"},"ipam":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.IPAM.Total() != 0 {
		t.Errorf("got %+v want zero", got.IPAM)
	}
	// Zero used with zero available is "nothing allocated", which *is* knowable.
	if !got.IPAM.ExhaustionKnown() {
		t.Error("an empty pool should count as known")
	}
}

func TestStripVersionExtras(t *testing.T) {
	tests := map[string]string{
		"1.16.5 abcdef1 2025-01-01 go1.23": "1.16.5",
		"1.15.8":                           "1.15.8",
		"":                                 "",
	}
	for in, want := range tests {
		if got := stripVersionExtras(in); got != want {
			t.Errorf("stripVersionExtras(%q): got %q want %q", in, got, want)
		}
	}
}

// --- flow rate ------------------------------------------------------------

// SeenFlows is a lifetime counter, so the rate has to be derived between polls. A
// lifetime average would understate a current spike by orders of magnitude on a
// long-running agent.
func TestFlowRate_DerivedBetweenPolls(t *testing.T) {
	p := &Plugin{}
	base := time.Now()

	// First sample has no predecessor, so there is no rate to report yet.
	if got := p.flowRate(1000, base); got != 0 {
		t.Errorf("first poll: got %v want 0", got)
	}
	// 500 flows over 10 seconds.
	if got := p.flowRate(1500, base.Add(10*time.Second)); got != 50 {
		t.Errorf("got %v want 50 flows/s", got)
	}
}

// A counter going backwards means the agent restarted; a negative rate would be
// nonsense, so it re-baselines instead.
func TestFlowRate_CounterResetRebaselines(t *testing.T) {
	p := &Plugin{}
	base := time.Now()
	p.flowRate(1_000_000, base)

	if got := p.flowRate(5, base.Add(10*time.Second)); got != 0 {
		t.Errorf("after a restart: got %v want 0", got)
	}
	// And the new baseline is used from then on.
	if got := p.flowRate(105, base.Add(20*time.Second)); got != 10 {
		t.Errorf("got %v want 10 flows/s", got)
	}
}

func TestFlowRate_ZeroElapsed(t *testing.T) {
	p := &Plugin{}
	at := time.Now()
	p.flowRate(100, at)
	if got := p.flowRate(200, at); got != 0 {
		t.Errorf("got %v want 0 for zero elapsed time", got)
	}
}

// --- settings -------------------------------------------------------------

func TestSettingsFrom(t *testing.T) {
	if got := SettingsFrom(nil); got != Defaults() {
		t.Errorf("got %+v want defaults", got)
	}
	got := SettingsFrom(map[string]any{
		"namespace": "cilium-system", "container": "agent", "pod_selector": "app=cilium",
	})
	if got.Namespace != "cilium-system" || got.Container != "agent" || got.PodSelector != "app=cilium" {
		t.Errorf("got %+v", got)
	}
	// The DaemonSet name was not overridden, so it keeps the default.
	if got.DaemonSetName != Defaults().DaemonSetName {
		t.Errorf("DaemonSetName: got %q", got.DaemonSetName)
	}
}

// --- banner ---------------------------------------------------------------

func TestCells(t *testing.T) {
	full := func(s Status) State {
		return State{Tier: kube.TierFull, AgentsReady: 6, AgentsDesired: 6, Status: s, UpdatedAt: time.Now()}
	}
	tests := []struct {
		name       string
		state      State
		wantStatus tui.BannerStatus
		wantDetail string
	}{
		{"healthy", full(Status{Controllers: Controllers{Total: 5}}), tui.BannerOK, ""},
		{
			// Agents down outranks everything: without an agent a node has no CNI.
			name:       "agents down",
			state:      State{Tier: kube.TierFull, AgentsReady: 4, AgentsDesired: 6},
			wantStatus: tui.BannerErr,
			wantDetail: "4/6",
		},
		{
			name:       "failing controllers",
			state:      full(Status{Controllers: Controllers{Total: 5, Failing: 2}}),
			wantStatus: tui.BannerWarn,
			wantDetail: "2 controller",
		},
		{
			// Pod-address exhaustion stops new pods scheduling and nothing else on
			// screen would explain why.
			name:       "ipam nearly exhausted",
			state:      full(Status{IPAM: IPAM{Used: 95, Available: 5}}),
			wantStatus: tui.BannerWarn,
			wantDetail: "IPAM 95%",
		},
		{
			// Unknown exhaustion must not be reported as full.
			name:       "unknown ipam is not an alarm",
			state:      full(Status{IPAM: IPAM{Used: 50}}),
			wantStatus: tui.BannerOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := store.New()
			s.Put(KeyState, tc.state)
			cells := (&Plugin{}).Cells(s)
			if len(cells) != 1 {
				t.Fatalf("got %d cells", len(cells))
			}
			if cells[0].Status != tc.wantStatus {
				t.Errorf("status: got %v want %v (detail %q)", cells[0].Status, tc.wantStatus, cells[0].Detail)
			}
			if tc.wantDetail != "" && !strings.Contains(cells[0].Detail, tc.wantDetail) {
				t.Errorf("detail: got %q want %q", cells[0].Detail, tc.wantDetail)
			}
		})
	}
}

func TestCells_NoStateYet(t *testing.T) {
	if got := (&Plugin{}).Cells(store.New()); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

// --- pane -----------------------------------------------------------------

func fullState(t *testing.T) State {
	t.Helper()
	status, err := ParseStatus(fixture(t, "1.16-summary-ipam.json"))
	if err != nil {
		t.Fatal(err)
	}
	return State{
		Tier: kube.TierFull, AgentsReady: 6, AgentsDesired: 6,
		Status: status, Pod: "cilium-abc12", UpdatedAt: time.Now(),
	}
}

func TestPane_FullTier(t *testing.T) {
	s := store.New()
	s.Put(KeyState, fullState(t))
	body := stripANSI(newPane(s).Render(70, 14, false))

	for _, want := range []string{"6/6 ready", "1.16.5", "kube-proxy", "42/256", "16%"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
	// Per-node figures must name the agent, or a reader takes one node's usage for
	// the cluster's.
	if !strings.Contains(body, "cilium-abc12") {
		t.Errorf("IPAM should be labeled with its pod:\n%s", body)
	}
}

// The informer tier says what is missing and why, rather than silently showing
// less and leaving the reader to wonder.
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

	// Agent readiness still shows: it needs no exec.
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

// An unknown IPAM shape shows the allocation count without inventing a
// percentage.
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

type errFake struct{}

func (errFake) Error() string { return "boom" }

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

func TestTierString(t *testing.T) {
	for tier, want := range map[kube.Tier]string{
		kube.TierAbsent:   "absent",
		kube.TierInformer: "informer-only",
		kube.TierFull:     "full",
	} {
		if got := tier.String(); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

// --- section isolation ----------------------------------------------------

// The real failure this guards: on a live cluster `hubble.observer.uptime` was a
// duration string rather than a number, and decoding the whole document as one
// struct aborted on it — losing the version, kube-proxy mode, IPAM and controller
// counts too, and dropping the plugin to informer-only.
//
// The field is no longer read at all, and sections decode independently, so this
// payload must now parse completely.
func TestParseStatus_StringUptimeDoesNotBreakTheDocument(t *testing.T) {
	got, err := ParseStatus(fixture(t, "string-uptime.json"))
	if err != nil {
		t.Fatalf("should parse: %v", err)
	}
	if got.Version != "1.16.5" {
		t.Errorf("Version: got %q", got.Version)
	}
	if got.KubeProxyReplacement != "True" {
		t.Errorf("KubeProxyReplacement: got %q", got.KubeProxyReplacement)
	}
	if got.IPAM.Used != 12 || got.IPAM.Available != 100 {
		t.Errorf("IPAM: got %+v want 12/100", got.IPAM)
	}
	if got.Controllers.Total != 1 {
		t.Errorf("Controllers: got %+v", got.Controllers)
	}
	// Hubble still decodes, since only the unread field had the odd type.
	if !got.Hubble.Enabled || got.Hubble.SeenFlows != 155951 {
		t.Errorf("Hubble: got %+v", got.Hubble)
	}
	if len(got.Unreadable) != 0 {
		t.Errorf("nothing should be unreadable: got %v", got.Unreadable)
	}
}

// A section of a genuinely wrong type must cost only itself, and must be named so
// a missing cell reads as "unreadable" rather than as absent or healthy.
func TestParseStatus_BrokenSectionIsIsolatedAndNamed(t *testing.T) {
	got, err := ParseStatus(fixture(t, "broken-section.json"))
	if err != nil {
		t.Fatalf("should still parse: %v", err)
	}
	// Everything else survives.
	if got.Version != "1.17.0" || got.KubeProxyReplacement != "True" {
		t.Errorf("surviving sections: got %+v", got)
	}
	if got.Controllers.Total != 1 || got.Hubble.SeenFlows != 42 {
		t.Errorf("surviving sections: got %+v", got)
	}
	// The broken one is zero and named.
	if got.IPAM.Total() != 0 {
		t.Errorf("broken section should be zero: got %+v", got.IPAM)
	}
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "ipam" {
		t.Errorf("Unreadable: got %v want [ipam]", got.Unreadable)
	}
}

// An explicit JSON null is absent, not unreadable.
func TestParseStatus_NullSectionIsNotUnreadable(t *testing.T) {
	got, err := ParseStatus([]byte(`{"cilium":{"version":"1.16.0"},"cluster-mesh":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unreadable) != 0 {
		t.Errorf("a null section is absent, not unreadable: got %v", got.Unreadable)
	}
}

// The pane must say which sections it could not read, so a shape change is
// distinguishable from a disabled feature.
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

// --- Cilium 1.19 ----------------------------------------------------------

// Reduced from a real 1.19.6 agent. Two things about that release defeated the
// parser: the version is in `cilium.msg` with no version field at all, and
// `ipam.ipv4` is a bare array of address *strings* while the pool total appears
// only in the human-readable `ipam.status` line.
func TestParseStatus_119(t *testing.T) {
	got, err := ParseStatus(fixture(t, "1.19-status-line-ipam.json"))
	if err != nil {
		t.Fatalf("should parse: %v", err)
	}

	// The version comes from msg, with the build suffix stripped.
	if got.Version != "1.19.6" {
		t.Errorf("Version: got %q want 1.19.6 (read from cilium.msg)", got.Version)
	}
	if got.State != "Ok" || got.KubeProxyReplacement != "True" {
		t.Errorf("got state=%q kube-proxy=%q", got.State, got.KubeProxyReplacement)
	}

	// The counts come from the status line, which is the only source of the total.
	if got.IPAM.Used != 124 || got.IPAM.Available != 130 {
		t.Errorf("IPAM: got %+v want 124 used / 130 available", got.IPAM)
	}
	if got.IPAM.Total() != 254 {
		t.Errorf("Total: got %d want 254", got.IPAM.Total())
	}
	if !got.IPAM.ExhaustionKnown() {
		t.Error("a total from the status line makes exhaustion knowable")
	}
	if got.IPAM.Percent() != 48 {
		t.Errorf("Percent: got %d want 48", got.IPAM.Percent())
	}

	if got.Controllers.Total != 1 || got.Hubble.SeenFlows != 155951 {
		t.Errorf("got %+v", got)
	}
	if len(got.Unreadable) != 0 {
		t.Errorf("nothing should be unreadable: got %v", got.Unreadable)
	}
}

// The version is read from msg only when there is no version field to prefer.
func TestParseStatus_VersionFieldWinsOverMsg(t *testing.T) {
	got, err := ParseStatus([]byte(
		`{"cilium":{"state":"Ok","version":"1.16.5","msg":"1.19.6 (something)"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.16.5" {
		t.Errorf("got %q want the version field to win", got.Version)
	}
}

// The status line is preferred over every structured field, because it is the only
// one carrying a total — and a used count without a total cannot answer "are we
// about to run out".
func TestIPAMFrom_StatusLinePreferred(t *testing.T) {
	got := ipamFrom(rawIPAM{
		Status: "IPv4: 10/100 allocated from 10.0.0.0/24, ",
		// A conflicting summary that must lose.
		Used:      999,
		Available: 999,
	})
	if got.Used != 10 || got.Available != 90 {
		t.Errorf("got %+v want 10/90 from the status line", got)
	}
}

func TestIPAMFrom_StatusLineVariants(t *testing.T) {
	tests := []struct {
		status        string
		wantUsed      int
		wantAvailable int
		wantParsed    bool
	}{
		{"IPv4: 124/254 allocated from 172.18.7.0/24, ", 124, 130, true},
		{"IPv4: 0/254 allocated from 10.0.0.0/24", 0, 254, true},
		{"IPv4: 254/254 allocated from 10.0.0.0/24", 254, 0, true},
		// Extra whitespace should not defeat it.
		{"IPv4:  7 / 20  allocated from 10.0.0.0/24", 7, 13, true},
		// Nonsense must not be believed.
		{"IPv6: 5/10 allocated", 0, 0, false},
		{"no numbers here", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range tests {
		got := ipamFrom(rawIPAM{Status: tc.status})
		if !tc.wantParsed {
			if got.Total() != 0 {
				t.Errorf("status %q should not parse, got %+v", tc.status, got)
			}
			continue
		}
		if got.Used != tc.wantUsed || got.Available != tc.wantAvailable {
			t.Errorf("status %q: got %+v want %d/%d", tc.status, got, tc.wantUsed, tc.wantAvailable)
		}
	}
}

// A used count larger than the total is not believable, so the line is ignored
// rather than producing a negative available.
func TestIPAMFrom_StatusLineRejectsImpossibleCounts(t *testing.T) {
	got := ipamFrom(rawIPAM{Status: "IPv4: 300/254 allocated from 10.0.0.0/24"})
	if got.Available < 0 {
		t.Errorf("Available must never be negative, got %+v", got)
	}
	if got.Total() != 0 {
		t.Errorf("an impossible line should be ignored, got %+v", got)
	}
}

// Without a status line, a bare array of address strings still yields a count —
// with exhaustion correctly reported as unknown.
func TestIPAMFrom_BareStringArrayCounts(t *testing.T) {
	got := ipamFrom(rawIPAM{IPv4: []byte(`["10.0.0.1","10.0.0.2","10.0.0.3"]`)})
	if got.Used != 3 {
		t.Errorf("Used: got %d want 3", got.Used)
	}
	if got.ExhaustionKnown() {
		t.Error("a bare address list gives no total, so exhaustion is unknown")
	}
}

// The tested-version list is reported to users in a parse-failure message, so it
// must actually correspond to the fixtures present.
func TestTestedVersionsMatchFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	joined := strings.Join(names, " ")
	for _, v := range TestedVersions {
		if !strings.Contains(joined, v) {
			t.Errorf("TestedVersions claims %q but no fixture is named for it: %v", v, names)
		}
	}
}

// "kube-proxy: True" reads as kube-proxy being present and enabled, when the
// field is kube-proxy-*replacement* and True means Cilium has taken over from it
// — on these clusters kube-proxy is not installed at all.
func TestKubeProxyText(t *testing.T) {
	tests := map[string]string{
		"True":     "replaced by Cilium",
		"true":     "replaced by Cilium",
		"Strict":   "replaced by Cilium",
		"False":    "not replaced",
		"Disabled": "not replaced",
		"Partial":  "partially replaced",
		// An unrecognized mode is shown verbatim rather than flattened into a
		// yes or no that might be wrong.
		"Probe":        "Probe",
		"SomethingNew": "SomethingNew",
		"":             "unknown",
	}
	for mode, want := range tests {
		if got := (Status{KubeProxyReplacement: mode}).KubeProxyText(); got != want {
			t.Errorf("KubeProxyText(%q) = %q, want %q", mode, got, want)
		}
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
	// The bare "True" beside a "kube-proxy" label is the misreading to avoid.
	if strings.Contains(body, "kube-proxy     True") {
		t.Errorf("pane still renders the raw mode:\n%s", body)
	}
}
