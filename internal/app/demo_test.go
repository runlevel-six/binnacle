package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/testansi"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// These assertions are about what a rendered cell contains, which needs a color
// profile that emits escape sequences.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// render draws one demo frame under the named theme.
func render(t *testing.T, theme, size string) string {
	t.Helper()
	setup, err := PrepareDemo(config.Config{Theme: theme})
	if err != nil {
		t.Fatalf("PrepareDemo(%q): %v", theme, err)
	}
	var buf bytes.Buffer
	w, h := 240, 54
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil {
		t.Fatalf("bad size %q: %v", size, err)
	}
	if err := RenderDemo(context.Background(), setup, &buf, w, h); err != nil {
		t.Fatalf("RenderDemo: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// A screenshot that changes between runs makes every regeneration a diff to
// review. The fixture holds durations rather than timestamps precisely so that
// this holds.
func TestRenderDemo_IsDeterministic(t *testing.T) {
	first := render(t, "", "240x54")
	second := render(t, "", "240x54")
	if first != second {
		t.Error("two renders of the same fixture differ; something in the frame is " +
			"reading a clock or iterating a map")
	}
}

// The demo has to actually demonstrate. A pane whose fixture key is missing draws
// an empty body or a "polling…" note, and a screenshot of that argues nothing —
// this is the check that catches a fixture gone stale against a changed model.
func TestRenderDemo_EveryPaneDraws(t *testing.T) {
	setup, err := PrepareDemo(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tui.ApplyTheme(setup.Resolved.Theme)
	setup.Registry.Detect(context.Background())

	model, err := setup.BuildModel()
	if err != nil {
		t.Fatal(err)
	}

	// Walk into groups: a merged frame holds several panes, and it is the members
	// that have fixture keys behind them. Counting frames would silently pass a
	// build where a group lost a member.
	var panes []tui.Pane
	for _, p := range model.Panes() {
		if g, ok := p.(*tui.GroupPane); ok {
			panes = append(panes, g.Members()...)
			continue
		}
		panes = append(panes, p)
	}
	// Ten, not eleven: Ceph contributes no grid pane, reporting through the
	// overview instead, which is what freed the column the OpenStack pane grew
	// into. A lower count means a plugin failed to detect.
	if len(panes) < 10 {
		t.Fatalf("demo built %d panes, want the full set — core plus the plugin panes; "+
			"a plugin that fails to detect contributes no pane at all", len(panes))
	}

	for _, p := range panes {
		body := testansi.StripANSI(p.Render(70, 12, false))
		if strings.TrimSpace(body) == "" {
			t.Errorf("pane %q renders nothing; its fixture key is missing", p.ID())
			continue
		}
		// A note in place of data means the pane found no snapshot, which for the
		// demo means the fixture never published one.
		for _, waiting := range []string{"polling", "waiting for", "loading"} {
			if strings.Contains(strings.ToLower(body), waiting) {
				t.Errorf("pane %q is still waiting for data: %q",
					p.ID(), strings.TrimSpace(body))
				break
			}
		}
	}
}

// The frame must be exactly the terminal it was given, under every theme, with
// the plugin panes present. The equivalent test in internal/ui can only reach the
// five core panes, because a plugin pane does not exist until a registry has
// detected its subsystem.
func TestRenderDemo_ExactDimensionsEveryTheme(t *testing.T) {
	for _, th := range tui.Themes() {
		for _, size := range []struct{ w, h int }{{80, 24}, {200, 50}, {240, 54}} {
			spec := fmt.Sprintf("%dx%d", size.w, size.h)
			out := render(t, th.Name, spec)

			lines := strings.Split(out, "\n")
			if len(lines) != size.h {
				t.Errorf("%s %s: %d lines, want %d", th.Name, spec, len(lines), size.h)
			}
			for i, line := range lines {
				if got := lipgloss.Width(line); got != size.w {
					t.Errorf("%s %s: line %d is %d cells, want %d",
						th.Name, spec, i, got, size.w)
				}
			}
		}
	}
}

// Every cell of a grounded theme's dashboard must be painted, plugin pane bodies
// included. Those bodies had never been rendered by any test when grounds landed.
func TestRenderDemo_GroundedThemesHaveNoHoles(t *testing.T) {
	for _, th := range tui.Themes() {
		if !th.Grounded() {
			continue
		}
		for _, spec := range []string{"80x24", "240x54"} {
			for i, line := range strings.Split(render(t, th.Name, spec), "\n") {
				testansi.AssertNoHoles(t, fmt.Sprintf("%s %s line %d", th.Name, spec, i), line)
			}
		}
	}
}

// And a theme that names one ground paints the whole dashboard with it.
func TestRenderDemo_SingleGroundIsSingle(t *testing.T) {
	for _, th := range tui.Themes() {
		if !th.Grounded() || th.ScreenBG() != th.PaneBG || th.HeaderBG != th.PaneBG {
			continue
		}
		for _, spec := range []string{"80x24", "240x54"} {
			for i, line := range strings.Split(render(t, th.Name, spec), "\n") {
				if got := testansi.Backgrounds(line); len(got) != 1 {
					t.Errorf("%s %s line %d: %d grounds %v, want one",
						th.Name, spec, i, len(got), got)
				}
			}
		}
	}
}

// The fixture must not leak anything that looks like a real deployment. Published
// screenshots come out of this path, and a hostname that slipped in would be
// permanent once pushed.
//
// Checked by category rather than against one operator's names. Naming the real
// domains here would put them in a public repository to keep them out of a
// screenshot, which defeats the purpose; and a category check also catches the
// next leak rather than only the one someone thought of.
func TestDemoFixture_UsesReservedNamesOnly(t *testing.T) {
	// Rendered tall enough that every pane shows its content. This test is about
	// what the fixture contains, not how it lays out, and at a short height a
	// merged frame legitimately clips the section carrying the addresses.
	body := testansi.StripANSI(render(t, "", "280x78"))

	// No routable or private address space: documentation ranges only.
	private := regexp.MustCompile(`\b(10\.\d{1,3}|192\.168|172\.(1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b`)
	if found := private.FindAllString(body, -1); found != nil {
		t.Errorf("demo frame contains private addresses %v; use 192.0.2.0/24 (RFC 5737)", found)
	}

	// No real-world hostname suffix. A fixture host belongs under .example
	// (RFC 6761), which cannot resolve for anyone.
	realTLD := regexp.MustCompile(`[a-z0-9-]+\.(com|net|org|io|dev|cloud|internal|local|lan)\b`)
	if found := realTLD.FindAllString(body, -1); found != nil {
		t.Errorf("demo frame contains real-looking hostnames %v; use .example", found)
	}

	// And the reserved markers must actually be in use, or the checks above are
	// passing over a fixture that has quietly stopped showing addresses at all.
	for _, want := range []string{"192.0.2.", ".example", "demo-management"} {
		if !strings.Contains(body, want) {
			t.Errorf("demo frame never shows %q; the fixture may have drifted", want)
		}
	}
}
