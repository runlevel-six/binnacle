package tui

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Styles only emit escape sequences when a color profile supports them, and
// several assertions here are about what a rendered cell actually contains.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// restoreTheme reinstates the default after a test that switches, since the
// palette is package state shared with every other test in the package.
func restoreTheme(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { ApplyTheme(DefaultTheme()) })
}

// Every glyph a theme supplies must be exactly one cell. A two-cell glyph makes
// a header cell wider than it measures, and a frame bar wider than the tile the
// layout sized for it.
func TestThemes_GlyphsAreSingleCell(t *testing.T) {
	for _, th := range Themes() {
		glyphs := map[string]string{
			"GlyphOK":      th.GlyphOK,
			"GlyphWarn":    th.GlyphWarn,
			"GlyphErr":     th.GlyphErr,
			"GlyphLoading": th.GlyphLoading,
		}
		if th.Frame == FrameBlock {
			glyphs["BarLeft"] = th.BarLeft
			glyphs["BarRight"] = th.BarRight
			glyphs["BarTop"] = th.BarTop
			glyphs["BarBottom"] = th.BarBottom
		}
		for name, g := range glyphs {
			if got := lipgloss.Width(g); got != 1 {
				t.Errorf("%s.%s = %q is %d cells wide, want 1", th.Name, name, g, got)
			}
		}
	}
}

// A block-framed theme has to supply its bars, or the frame renders as spaces.
func TestThemes_BlockFramesDefineBars(t *testing.T) {
	for _, th := range Themes() {
		if th.Frame != FrameBlock {
			continue
		}
		if th.BarLeft == "" || th.BarRight == "" || th.BarTop == "" || th.BarBottom == "" {
			t.Errorf("%s uses FrameBlock but leaves a bar glyph empty", th.Name)
		}
	}
}

// Every theme must set every color. A zero lipgloss.Color renders as the
// terminal default, which reads as "unstyled" — so a forgotten field shows up as
// a pane that has quietly lost its status coloring, not as an obvious mistake.
func TestThemes_NoMissingColours(t *testing.T) {
	for _, th := range Themes() {
		fields := map[string]lipgloss.Color{
			"OK": th.OK, "Warn": th.Warn, "Err": th.Err, "Muted": th.Muted,
			"Accent": th.Accent, "Header": th.Header, "Border": th.Border,
			"Focus": th.Focus, "Title": th.Title, "TitleDim": th.TitleDim,
			"HeaderBG": th.HeaderBG, "HeaderFG": th.HeaderFG,
		}
		for name, c := range fields {
			if c == "" {
				t.Errorf("%s.%s is unset", th.Name, name)
			}
		}
		if th.Separator == "" {
			t.Errorf("%s.Separator is unset", th.Name)
		}
		if th.Description == "" {
			t.Errorf("%s has no description for --list-themes", th.Name)
		}
	}
}

// A theme grounds itself completely or not at all. Half of it is the bad case:
// panes painted but the screen behind them left bare reads as a rendering fault,
// and a ground with no ink of its own leaves ordinary body rows in whatever
// foreground the terminal defaults to — invisible, on the wrong terminal.
func TestThemes_GroundedThemesAreFullyGrounded(t *testing.T) {
	for _, th := range Themes() {
		if !th.Grounded() {
			// And an ungrounded theme must not carry half a ground either.
			if th.AppBG != "" || th.Text != "" {
				t.Errorf("%s sets AppBG or Text but no PaneBG, so it is not grounded "+
					"and those will be ignored", th.Name)
			}
			// Nor may it leave the screen a color it will never paint.
			if th.ScreenBG() != "" {
				t.Errorf("%s is ungrounded but ScreenBG() = %v", th.Name, th.ScreenBG())
			}
			continue
		}
		if th.Text == "" {
			t.Errorf("%s grounds its panes but supplies no Text ink", th.Name)
		}
		if th.ScreenBG() == "" {
			t.Errorf("%s grounds its panes but leaves the screen behind them unset", th.Name)
		}
	}
}

// One ground unless a theme asks for two. A backdrop that silently differs from
// the panes reads as an unfilled gap, so the fallback is the pane ground and an
// AppBG has to be deliberate.
func TestScreenBG_FallsBackToPaneGround(t *testing.T) {
	if got, want := NcursesTheme().ScreenBG(), NcursesTheme().PaneBG; got != want {
		t.Errorf("ScreenBG() = %v, want the pane ground %v", got, want)
	}
	distinct := NcursesTheme()
	distinct.AppBG = lipgloss.Color("#123456")
	if got := distinct.ScreenBG(); got != lipgloss.Color("#123456") {
		t.Errorf("an explicit AppBG should win, got %v", got)
	}
}

// A grounded theme's body styles must each carry the pane background. A span
// that sets only a foreground ends with a reset that clears the ground, which
// punches a hole in the panel exactly where a status color sits.
func TestApplyTheme_GroundedStylesCarryTheGround(t *testing.T) {
	restoreTheme(t)
	th := NcursesTheme()
	ApplyTheme(th)

	for name, s := range map[string]lipgloss.Style{
		"StyleOK": StyleOK, "StyleWarn": StyleWarn, "StyleErr": StyleErr,
		"StyleMuted": StyleMuted, "StyleAccent": StyleAccent, "StyleHeader": StyleHeader,
	} {
		if got := s.GetBackground(); got != th.PaneBG {
			t.Errorf("%s background = %v, want the pane ground %v", name, got, th.PaneBG)
		}
	}
}

// An ungrounded theme must gain no background at all, or every existing theme
// would start painting over the terminal's own.
func TestApplyTheme_UngroundedStylesStayTransparent(t *testing.T) {
	restoreTheme(t)
	ApplyTheme(DefaultTheme())
	for name, s := range map[string]lipgloss.Style{
		"StyleOK": StyleOK, "StyleMuted": StyleMuted, "StyleAccent": StyleAccent,
	} {
		if _, unset := s.GetBackground().(lipgloss.NoColor); !unset {
			t.Errorf("%s picked up a background under an ungrounded theme: %v",
				name, s.GetBackground())
		}
	}
}

func TestBoxBorder_DefaultsToRounded(t *testing.T) {
	if got := DefaultTheme().BoxBorder(); got != lipgloss.RoundedBorder() {
		t.Errorf("a theme naming no box should draw the rounded one, got %+v", got)
	}
	if got := NcursesTheme().BoxBorder(); got != lipgloss.DoubleBorder() {
		t.Errorf("ncurses should draw the double box, got %+v", got)
	}
}

func TestThemes_NamesUniqueAndDefaultFirst(t *testing.T) {
	names := ThemeNames()
	if len(names) == 0 || names[0] != DefaultTheme().Name {
		t.Errorf("the default theme should lead the catalog, got %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate theme name %q", n)
		}
		seen[n] = true
		if n != strings.ToLower(n) || strings.ContainsAny(n, " \t") {
			t.Errorf("theme name %q is not a clean flag value", n)
		}
	}
}

// The package palette starts out matching the default theme, so a caller that
// never mentions themes still gets a styled dashboard.
func TestApplyTheme_DefaultIsInstalledAtInit(t *testing.T) {
	def := DefaultTheme()
	if CurrentTheme().Name != def.Name {
		t.Errorf("current theme at init: got %q want %q", CurrentTheme().Name, def.Name)
	}
	if ColorOK != def.OK || GlyphOK != def.GlyphOK {
		t.Error("the package palette was not initialized from the default theme")
	}
	if StyleOK.GetForeground() != def.OK {
		t.Error("StyleOK was not built from the default theme")
	}
}

func TestApplyTheme_RewritesPalette(t *testing.T) {
	restoreTheme(t)
	lcars := LCARSTheme()
	ApplyTheme(lcars)

	if CurrentTheme().Name != lcars.Name {
		t.Fatalf("current theme: got %q want %q", CurrentTheme().Name, lcars.Name)
	}
	for _, tc := range []struct {
		name      string
		got, want lipgloss.Color
	}{
		{"ColorOK", ColorOK, lcars.OK},
		{"ColorWarn", ColorWarn, lcars.Warn},
		{"ColorErr", ColorErr, lcars.Err},
		{"ColorMuted", ColorMuted, lcars.Muted},
		{"ColorAccent", ColorAccent, lcars.Accent},
		{"ColorHeader", ColorHeader, lcars.Header},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, tc.got, tc.want)
		}
	}
	if StyleErr.GetForeground() != lcars.Err {
		t.Errorf("StyleErr fg: got %v want %v", StyleErr.GetForeground(), lcars.Err)
	}
	if GlyphOK != lcars.GlyphOK {
		t.Errorf("GlyphOK: got %q want %q", GlyphOK, lcars.GlyphOK)
	}

	// The status mapping reads the styles rather than caching them, which is
	// what carries a theme into every pane without the panes knowing.
	if StatusStyle("Running").GetForeground() != lcars.OK {
		t.Error("StatusStyle should follow the applied theme")
	}
	// And the health strip's glyphs follow too.
	if glyph, _ := Glyph(BannerOK); glyph != lcars.GlyphOK {
		t.Errorf("banner glyph: got %q want %q", glyph, lcars.GlyphOK)
	}
}

func TestLookupTheme(t *testing.T) {
	if th, err := LookupTheme(""); err != nil || th.Name != DefaultTheme().Name {
		t.Errorf("empty name should select the default: %q, %v", th.Name, err)
	}
	// Tolerant of how a name arrives from a shell or a YAML file.
	for _, name := range []string{"lcars", "LCARS", " lcars ", "Lcars"} {
		th, err := LookupTheme(name)
		if err != nil || th.Name != "lcars" {
			t.Errorf("LookupTheme(%q): got %q, %v", name, th.Name, err)
		}
	}
}

// An unknown name must fail loudly and say what would have worked. Falling back
// to the default would leave someone staring at an unchanged dashboard,
// wondering whether the flag or the theme was broken.
func TestLookupTheme_UnknownNamesAlternatives(t *testing.T) {
	_, err := LookupTheme("lcras")
	if err == nil {
		t.Fatal("an unknown theme should be an error")
	}
	for _, want := range append([]string{"lcras"}, ThemeNames()...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestNextTheme(t *testing.T) {
	names := ThemeNames()
	for i, name := range names {
		want := names[(i+1)%len(names)]
		if got := NextTheme(name).Name; got != want {
			t.Errorf("NextTheme(%q): got %q want %q", name, got, want)
		}
	}
	// Cycling from something unrecognized must still land somewhere usable.
	if got := NextTheme("nonsense").Name; got != names[0] {
		t.Errorf("NextTheme(unknown): got %q want %q", got, names[0])
	}
}

func TestFrameColor(t *testing.T) {
	plain := DefaultTheme()
	if got := plain.FrameColor(3, false); got != plain.Border {
		t.Errorf("a theme without accents should use Border, got %v", got)
	}
	if got := plain.FrameColor(3, true); got != plain.Focus {
		t.Errorf("focused: got %v want %v", got, plain.Focus)
	}

	accented := LCARSTheme()
	if got := accented.FrameColor(0, true); got != accented.Focus {
		t.Error("focus must win over the accent list, or focus becomes invisible")
	}
	n := len(accented.Accents)
	if got, want := accented.FrameColor(n+1, false), accented.Accents[1]; got != want {
		t.Errorf("accents should cycle: got %v want %v", got, want)
	}
	// Index arithmetic must not panic or fall off the list.
	for _, i := range []int{-1, -n, -n - 3, 0, 1000} {
		if got := accented.FrameColor(i, false); got == "" {
			t.Errorf("FrameColor(%d) returned no color", i)
		}
	}
}

func TestLabelAndPaneTitle(t *testing.T) {
	def := DefaultTheme()
	if got := def.Label("cluster"); got != "cluster" {
		t.Errorf("the default theme should leave labels alone, got %q", got)
	}
	if got := def.PaneTitle("nodes", "Nodes"); got != "Nodes" {
		t.Errorf("the default theme should leave titles alone, got %q", got)
	}

	lcars := LCARSTheme()
	if got := lcars.Label("cluster"); got != "CLUSTER" {
		t.Errorf("LCARS should shout labels, got %q", got)
	}
	title := lcars.PaneTitle("nodes", "Nodes")
	if !strings.HasPrefix(title, "NODES ") {
		t.Errorf("LCARS pane title: got %q", title)
	}
	if !regexp.MustCompile(`^NODES \d{2}-\d{3}$`).MatchString(title) {
		t.Errorf("LCARS pane title should carry a catalog number, got %q", title)
	}
	// Hashed, not random: the same pane wears the same number every run, so the
	// number never reads as data that changed.
	if again := lcars.PaneTitle("nodes", "Nodes"); again != title {
		t.Errorf("pane tag is unstable: %q then %q", title, again)
	}
	if other := lcars.PaneTitle("pods", "Nodes"); other == title {
		t.Error("different panes should get different catalog numbers")
	}
}

// A cell drawn on the header bar must not punch a hole in it. A style that sets
// no background emits a reset that clears the enclosing one, and lipgloss does
// not re-arm it afterwards.
func TestBannerCellRender_InheritsBackground(t *testing.T) {
	bg := lipgloss.Color("236")
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(bg)

	out := RenderCell(BannerCell{Name: "Nodes", Status: BannerWarn, Detail: "2 moving"}, nameStyle)
	// Every span between the resets has to re-establish the background, so the
	// count of background introductions matches the count of resets.
	if got, want := strings.Count(out, "48;5;236"), strings.Count(out, "\x1b[0m"); got != want {
		t.Errorf("background armed %d times for %d resets:\n%q", got, want, out)
	}

	// A caller with no background gets no background: the cell must not invent
	// one for a pane body that is not drawing a bar.
	plain := RenderCell(BannerCell{Name: "Nodes", Status: BannerWarn}, lipgloss.Style{})
	if strings.Contains(plain, "48;5;") {
		t.Errorf("an unbacked cell should not set a background:\n%q", plain)
	}
}
