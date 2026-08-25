package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/internal/build"
	"github.com/runlevel-six/sextant/internal/config"
	coremodel "github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/testansi"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// These assertions are about what a rendered cell contains, which needs a color
// profile that emits escape sequences.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// demoVersion is the build every rendered demo frame claims to be, so that a
// frame is evidence about the assembled header rather than about whatever the
// test binary was linked with.
const demoVersion = "1.4.0"

// render draws one demo frame under the named theme.
func render(t *testing.T, theme, size string) string {
	t.Helper()
	setup, err := PrepareDemo(config.Config{Theme: theme}, build.Info{Version: demoVersion})
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
	setup, err := PrepareDemo(config.Config{}, build.Info{})
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

// Which build is on screen is the first thing a bug report has to establish, and
// the header is what gets screenshotted. This walks the whole wiring the version
// travels — the linker's metadata through PrepareDemo, BuildModel and into the
// assembled frame — which no test inside internal/ui can reach.
func TestRenderDemo_HeaderNamesTheBuild(t *testing.T) {
	first := strings.SplitN(testansi.StripANSI(render(t, "", "240x54")), "\n", 2)[0]
	if !strings.Contains(first, "v"+demoVersion) {
		t.Errorf("the header's first row should name the build: %q", first)
	}
	// Beside the name, not adrift at the other end of the row: the far right is
	// where the row truncates and where the rollout badges go.
	if name, version := strings.Index(strings.ToLower(first), "sextant"),
		strings.Index(first, "v"+demoVersion); version-name > len("sextant ") {
		t.Errorf("the version should sit next to the tool's name: %q", first)
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

// A pane given the height it declared must not have to hide anything in it. This
// is the direction of error that matters: a ceiling that overstates costs a blank
// line, while one that understates makes the layout confidently hand back space
// the pane needed, and the reader sees "+ 3 more" in a pane that asked to be
// exactly this tall.
//
// Checked through the assembled demo — frames included, since a merged frame's
// ceiling is composed from its members' and is the one the layout actually reads.
func TestRenderDemo_DeclaredCeilingsHideNothing(t *testing.T) {
	setup, err := PrepareDemo(config.Config{}, build.Info{})
	if err != nil {
		t.Fatal(err)
	}
	tui.ApplyTheme(setup.Resolved.Theme)
	setup.Registry.Detect(context.Background())

	model, err := setup.BuildModel()
	if err != nil {
		t.Fatal(err)
	}

	hidden := regexp.MustCompile(`\+ \d+ more`)
	declared := 0
	for _, p := range model.Panes() {
		ch, ok := p.(tui.ContentHeightPane)
		if !ok {
			continue
		}
		// At the width the pane asked for, since a ceiling is measured at one.
		w := 80
		if cw, sized := p.(tui.ContentWidthPane); sized && cw.ContentWidth() > 0 {
			w = cw.ContentWidth()
		}
		h := ch.ContentHeight(w)
		if h <= 0 {
			continue // no ceiling declared for this data
		}
		declared++

		body := testansi.StripANSI(p.Render(w, h, false))
		if got := hidden.FindString(body); got != "" {
			t.Errorf("pane %q declared a ceiling of %d lines at width %d, then hid rows "+
				"behind %q:\n%s", p.ID(), h, w, got, body)
		}
		if lines := strings.Count(body, "\n") + 1; lines > h {
			t.Errorf("pane %q overran its own ceiling: %d lines in %d", p.ID(), lines, h)
		}
	}
	if declared == 0 {
		t.Fatal("no pane declared a content ceiling; the demo should exercise several")
	}
}

// A dashboard that re-measures itself on every store update has to converge, or
// the reader is chasing a moving screen. This drives the full plugin-bearing demo
// through repeated polls of the churn a real cluster produces — a pod crashing and
// recovering, events arriving under a new object each time — and requires the
// arrangement to stop moving and stay stopped.
//
// Alternating rather than random on purpose: the failure this guards against is
// oscillation, where two data states each look worth resizing for and the screen
// swings between them for as long as the condition lasts.
func TestRenderDemo_LayoutConvergesUnderChurn(t *testing.T) {
	for _, size := range []struct{ w, h int }{{395, 111}, {320, 80}, {240, 60}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			setup, err := PrepareDemo(config.Config{}, build.Info{})
			if err != nil {
				t.Fatal(err)
			}
			tui.ApplyTheme(setup.Resolved.Theme)
			setup.Registry.Detect(context.Background())
			model, err := setup.BuildModel()
			if err != nil {
				t.Fatal(err)
			}
			model.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})

			var frames []string
			for round := range 12 {
				churn(setup.Store, round%2 == 1)
				view := model.View()
				// Held geometry is a whole previous layout, so it still has to
				// tile the terminal exactly — a frame that shrank would leave the
				// last one on screen underneath it.
				if got := lipgloss.Height(view); got != size.h {
					t.Fatalf("poll %d: frame height %d, want %d", round, got, size.h)
				}
				for i, line := range strings.Split(view, "\n") {
					if got := lipgloss.Width(line); got != size.w {
						t.Fatalf("poll %d: line %d width %d, want %d", round, i, got, size.w)
					}
				}
				frames = append(frames, borderSignature(view))
			}

			// Two frames of grace: the first sight of each state may legitimately
			// need the width. After that the screen must hold still.
			for i := 2; i < len(frames); i++ {
				if frames[i] != frames[2] {
					t.Fatalf("the layout was still moving at poll %d\n  poll 2: %s\n  poll %d: %s",
						i, frames[2], i, frames[i])
				}
			}
		})
	}
}

// churn replaces the pods and events with one of two states a cluster alternates
// between: a crash-looping pod with a long name that comes and goes, and the next
// poll's events, which name different objects.
func churn(s *store.Store, crashing bool) {
	now := time.Now()
	pods := []coremodel.Pod{
		{Namespace: "kube-system", Name: "coredns-6f8b4d9c7-2xk9v", IsHealthy: true,
			ReadyReady: 1, ReadyTotal: 1, Status: "Running", Node: "control-node-1.site-a.demo.example"},
		{Namespace: "monitoring", Name: "prometheus-server-0", ReadyTotal: 2, Status: "Pending"},
	}
	events := []coremodel.Event{
		{Namespace: "kube-system", Type: "Warning", Reason: "BackOff", ObjectKind: "Pod",
			ObjectName: "coredns-6f8b4d9c7-2xk9v", Message: "Back-off restarting failed container",
			LastTimestamp: now, Count: 3},
	}
	if crashing {
		pods = append(pods, coremodel.Pod{
			Namespace: "openstack", Name: "nova-compute-5d9c7b8f4d-xk2mn", ReadyTotal: 3,
			Status: "CrashLoopBackOff", Restarts: 9, Node: "compute-node-3.site-a.demo.example"})
		events = append(events, coremodel.Event{
			Namespace: "openstack", Type: "Warning", Reason: "BackOff", ObjectKind: "Pod",
			ObjectName:    "nova-compute-5d9c7b8f4d-xk2mn-with-a-much-longer-name",
			Message:       "Back-off restarting failed container nova-compute",
			LastTimestamp: now, Count: 41})
	}
	s.Put(coremodel.KeyWorkloadPods, coremodel.Snapshot[coremodel.Pod]{Items: pods, UpdatedAt: now})
	s.Put(coremodel.KeyWorkloadEvents, coremodel.Snapshot[coremodel.Event]{Items: events, UpdatedAt: now})
	s.Put(coremodel.KeyMgmtEvents, coremodel.Snapshot[coremodel.Event]{Items: events, UpdatedAt: now})
}

// borderSignature is where every tile corner sits, which is the whole arrangement
// and none of the content.
func borderSignature(frame string) string {
	var b strings.Builder
	for i, line := range strings.Split(testansi.StripANSI(frame), "\n") {
		var cols []string
		for c, r := range []rune(line) {
			if strings.ContainsRune("╭╮╰╯├┤┬┴┼", r) {
				cols = append(cols, fmt.Sprint(c))
			}
		}
		if len(cols) > 0 {
			fmt.Fprintf(&b, "%d:%s ", i, strings.Join(cols, ","))
		}
	}
	return b.String()
}
