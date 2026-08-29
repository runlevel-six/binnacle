package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestPriorityString(t *testing.T) {
	for p, want := range map[Priority]string{
		P0Critical:  "P0Critical",
		P1Important: "P1Important",
		P2Useful:    "P2Useful",
		P3Optional:  "P3Optional",
		Priority(9): "P?",
	} {
		if got := p.String(); got != want {
			t.Errorf("Priority(%d).String(): got %q want %q", p, got, want)
		}
	}
}

// Priorities must stay ordered lowest-is-most-important, since the layout sorts
// on the numeric value.
func TestPriorityOrdering(t *testing.T) {
	if P0Critical >= P1Important || P1Important >= P2Useful || P2Useful >= P3Optional {
		t.Error("priority constants are not in ascending order of numeric value")
	}
}

func TestStatusStyle(t *testing.T) {
	tests := []struct {
		status string
		want   lipgloss.Style
	}{
		{"Ready", StyleOK},
		{"Running", StyleOK},
		{"Completed", StyleOK},
		{"Succeeded", StyleOK},
		{"NotReady", StyleErr},
		{"CrashLoopBackOff", StyleErr},
		{"OOMKilled", StyleErr},
		{"Pending", StyleWarn},
		{"Terminating", StyleWarn},
		{"Cordoned", StyleWarn},
		{"SchedulingDisabled", StyleWarn},
		{"Ready,SchedulingDisabled", StyleWarn},
		{"Init:0/2", StyleWarn},
		{"Init:CrashLoopBackOff", StyleWarn},
		// Unrecognized statuses read as "no opinion", not as a failure.
		{"SomethingBrandNew", StyleMuted},
		{"", StyleMuted},
	}
	for _, tc := range tests {
		got := StatusStyle(tc.status)
		if got.GetForeground() != tc.want.GetForeground() {
			t.Errorf("StatusStyle(%q): got fg %v want %v",
				tc.status, got.GetForeground(), tc.want.GetForeground())
		}
	}
}

// "Init:" is a prefix match, so a short string starting with fewer than five
// characters must not be mistaken for it.
func TestStatusStyle_ShortStringsDoNotPanic(t *testing.T) {
	for _, s := range []string{"I", "In", "Ini", "Init", "Init:"} {
		_ = StatusStyle(s)
	}
}

func TestHasStyle(t *testing.T) {
	if HasStyle(lipgloss.Style{}) {
		t.Error("zero style should report no styling")
	}
	if !HasStyle(StyleOK) {
		t.Error("a foreground color should count as styling")
	}
	if !HasStyle(lipgloss.NewStyle().Bold(true)) {
		t.Error("bold should count as styling")
	}
}

func TestBannerStatusString(t *testing.T) {
	for s, want := range map[BannerStatus]string{
		BannerLoading:   "loading",
		BannerOK:        "ok",
		BannerWarn:      "warn",
		BannerErr:       "err",
		BannerStatus(9): "?",
	} {
		if got := s.String(); got != want {
			t.Errorf("BannerStatus(%d).String(): got %q want %q", s, got, want)
		}
	}
}

// BannerLoading must be the zero value so a cell built before its data arrives
// reads as unknown rather than healthy.
func TestBannerLoadingIsZeroValue(t *testing.T) {
	var zero BannerStatus
	if zero != BannerLoading {
		t.Errorf("zero BannerStatus is %v, want BannerLoading", zero)
	}
	var cell BannerCell
	if cell.Status != BannerLoading {
		t.Error("zero BannerCell should report loading, not ok")
	}
}

func TestBannerStatusWorse(t *testing.T) {
	tests := []struct {
		a, b, want BannerStatus
	}{
		{BannerOK, BannerWarn, BannerWarn},
		{BannerWarn, BannerOK, BannerWarn},
		{BannerWarn, BannerErr, BannerErr},
		{BannerErr, BannerLoading, BannerErr},
		{BannerLoading, BannerOK, BannerOK},
		{BannerOK, BannerOK, BannerOK},
	}
	for _, tc := range tests {
		if got := tc.a.Worse(tc.b); got != tc.want {
			t.Errorf("%v.Worse(%v): got %v want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestBannerStatusGlyph(t *testing.T) {
	for _, s := range []BannerStatus{BannerLoading, BannerOK, BannerWarn, BannerErr} {
		glyph, _ := Glyph(s)
		if glyph == "" {
			t.Errorf("BannerStatus(%v) has no glyph", s)
		}
		if lipgloss.Width(glyph) != 1 {
			t.Errorf("BannerStatus(%v) glyph %q is %d cells wide, want 1", s, glyph, lipgloss.Width(glyph))
		}
	}
}

func TestBannerCellRender(t *testing.T) {
	plain := lipgloss.Style{}

	healthy := RenderCell(BannerCell{Name: "Nodes", Status: BannerOK}, plain)
	if !strings.Contains(healthy, "Nodes") {
		t.Errorf("render dropped the name: %q", healthy)
	}

	// A healthy cell stays terse; detail only appears when set.
	degraded := RenderCell(BannerCell{Name: "Nodes", Status: BannerWarn, Detail: "2 NotReady"}, plain)
	if !strings.Contains(degraded, "2 NotReady") {
		t.Errorf("render dropped the detail: %q", degraded)
	}
	if lipgloss.Width(healthy) >= lipgloss.Width(degraded) {
		t.Error("a healthy cell should render narrower than a degraded one")
	}
}

// PaneChromeH/V are subtracted by layout code, so their relationship to the
// border model is load-bearing.
func TestChromeConstants(t *testing.T) {
	if PaneChromeH != 4 {
		t.Errorf("PaneChromeH: got %d want 4 (2 border + 2 padding)", PaneChromeH)
	}
	if PaneChromeV != 2 {
		t.Errorf("PaneChromeV: got %d want 2 (top + bottom border)", PaneChromeV)
	}
}
