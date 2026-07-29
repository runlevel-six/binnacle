package tui

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// FrameStyle selects how a pane's border is drawn.
type FrameStyle int

const (
	// FrameRounded draws a one-cell box-drawing border with the title inlaid
	// in the top edge.
	FrameRounded FrameStyle = iota
	// FrameBlock draws solid bars: a heavy rail down each side and half-block
	// bars hugging the content top and bottom. Panes are colored from
	// [Theme.Accents] rather than sharing one border color.
	FrameBlock
)

// Theme is a complete color and glyph scheme for the dashboard.
//
// It covers three things that have to agree with each other: the semantic body
// palette every pane and plugin draws with, the chrome the program draws around
// them, and the glyphs both use. Splitting those across two packages is how a
// theme ends up half-applied, so they live in one struct.
//
// Colors are given as hex. lipgloss degrades them to the 256-color cube or to
// the basic sixteen according to the profile it detects, so a hex theme still
// renders on a terminal that cannot do truecolor — it does not require it.
type Theme struct {
	// Name is the identifier accepted by --theme. Lowercase, no spaces.
	Name string
	// Description is the one-line summary shown by --list-themes.
	Description string

	// Body palette, mirrored into the package-level Color* and Style* variables
	// by [ApplyTheme].
	OK     lipgloss.Color
	Warn   lipgloss.Color
	Err    lipgloss.Color
	Muted  lipgloss.Color
	Accent lipgloss.Color
	Header lipgloss.Color

	// Chrome: the frame around a pane and the header and footer bars. Read
	// directly by the program; panes have no business drawing chrome.
	Border   lipgloss.Color
	Focus    lipgloss.Color
	Title    lipgloss.Color
	TitleDim lipgloss.Color
	HeaderBG lipgloss.Color
	HeaderFG lipgloss.Color

	// Ground colors, for a theme that paints the screen rather than letting
	// the terminal's own background show through.
	//
	// PaneBG grounds a pane's interior and its border cells, and Text is the ink
	// for body content that carries no semantic color of its own — a ground
	// needs one, because a pane's ordinary rows are written bare and would
	// otherwise be drawn in whatever foreground the terminal happens to default
	// to, which on a light panel can be nothing at all. Those two are what makes
	// a theme grounded; see [Theme.Grounded].
	//
	// AppBG is an optional second ground for the screen outside the panes, and
	// defaults to PaneBG. A theme wants one ground unless it has a reason to
	// want two: the space between panes is not a different surface, and painting
	// it a different color reads as an unfilled gap rather than as a choice.
	// Set it only to make the backdrop deliberately distinct. See
	// [Theme.ScreenBG].
	PaneBG lipgloss.Color
	AppBG  lipgloss.Color
	Text   lipgloss.Color

	// Frame selects the pane border style.
	Frame FrameStyle
	// Box overrides the glyphs a [FrameRounded] frame is drawn with. The zero
	// value draws lipgloss's rounded box, which is what sextant has always
	// used; a theme wanting the double-line box of a curses dialog sets
	// [lipgloss.DoubleBorder] here.
	Box lipgloss.Border
	// Accents, when non-empty, colors each pane's frame by its position in
	// focus order instead of giving every unfocused pane [Theme.Border]. The
	// focused pane always uses [Theme.Focus], so it still stands out.
	Accents []lipgloss.Color

	// Bar* are the frame glyphs used when Frame is [FrameBlock]. Each must be
	// exactly one cell wide, or the frame will not match the width the layout
	// allotted it.
	BarLeft   string
	BarRight  string
	BarTop    string
	BarBottom string

	// Status glyphs for the health strip.
	GlyphOK      string
	GlyphWarn    string
	GlyphErr     string
	GlyphLoading string

	// Separator joins the fields of the identity line and the footer.
	Separator string

	// UpperLabels uppercases chrome text: pane titles, field labels, the tool
	// name. It never touches data — a context, profile or cluster name is an
	// identifier, and shouting it would misrepresent what it is.
	UpperLabels bool
	// TagTitles appends a stable pseudo-catalog number to each pane title, as
	// an LCARS panel carries. Cosmetic, and derived from the pane ID so it does
	// not change between runs.
	TagTitles bool
}

// DefaultTheme is the palette sextant has always shipped: 256-color indices,
// rounded borders, and a green/amber/red health scheme.
func DefaultTheme() Theme {
	return Theme{
		Name:        "default",
		Description: "green/amber/red on rounded borders",

		OK:     lipgloss.Color("76"),
		Warn:   lipgloss.Color("214"),
		Err:    lipgloss.Color("196"),
		Muted:  lipgloss.Color("244"),
		Accent: lipgloss.Color("141"),
		Header: lipgloss.Color("250"),

		Border:   lipgloss.Color("240"),
		Focus:    lipgloss.Color("39"),
		Title:    lipgloss.Color("250"),
		TitleDim: lipgloss.Color("244"),
		HeaderBG: lipgloss.Color("236"),
		HeaderFG: lipgloss.Color("250"),

		Frame: FrameRounded,

		GlyphOK:      "✓",
		GlyphWarn:    "⚠",
		GlyphErr:     "✗",
		GlyphLoading: "—",

		Separator: "  ·  ",
	}
}

// LCARSTheme is the Star Trek: The Next Generation library computer look:
// black ground, panels railed in orange, violet, gold and ice blue, and
// shouting block capitals.
//
// The black is load-bearing rather than incidental. An LCARS panel is a colored
// block on an unlit screen — the black is the display, and the gaps between the
// rails are that same black showing through, which is why the look does not
// survive being dropped on a pale terminal. This theme therefore grounds itself:
// [Theme.PaneBG] is true black everywhere, header included, and Text is the warm
// off-white those readouts are lettered in. Before grounds existed the theme
// could only ask for the black and hope the terminal already had it.
//
// The colors are the documented LCARS set — Neon Carrot, Golden Tanoi, African
// Violet, Anakiwa, Blue Bell. One deliberate departure, because a dashboard has
// to be read under pressure: alert red is brighter than the canonical Chestnut
// Rose so a failed host is unmistakable.
//
// There is no header band. The identity line and health strip are lettered
// straight onto the black, as an LCARS readout is; a solid orange bar would be
// the authentic furniture but amber-on-orange is the exact combination that makes
// a rollout warning invisible.
func LCARSTheme() Theme {
	return Theme{
		Name:        "lcars",
		Description: "LCARS-style console: black ground, block rails, amber and violet",

		// Nominal reads as ice blue rather than green: LCARS has no green, and
		// blue against amber and violet is still the clearest "fine" signal.
		OK:     lipgloss.Color("#99CCFF"),
		Warn:   lipgloss.Color("#FFCC66"),
		Err:    lipgloss.Color("#FF4D4D"),
		Muted:  lipgloss.Color("#9999CC"),
		Accent: lipgloss.Color("#FF9900"),
		Header: lipgloss.Color("#FFCC66"),

		Border:   lipgloss.Color("#CC99CC"),
		Focus:    lipgloss.Color("#FF9900"),
		Title:    lipgloss.Color("#FFCC99"),
		TitleDim: lipgloss.Color("#CC99CC"),
		HeaderBG: lipgloss.Color("#000000"),
		HeaderFG: lipgloss.Color("#FF9900"),

		// One unlit screen, no AppBG: the black between the rails is the same
		// black the rails sit on.
		PaneBG: lipgloss.Color("#000000"),
		Text:   lipgloss.Color("#FFCC99"),

		Frame: FrameBlock,
		// Neon Carrot is absent on purpose: it is the focus color, and a
		// resting pane wearing it would compete with the focused one.
		Accents: []lipgloss.Color{
			lipgloss.Color("#CC99CC"), // African Violet
			lipgloss.Color("#99CCFF"), // Anakiwa
			lipgloss.Color("#FFCC66"), // Golden Tanoi
			lipgloss.Color("#9999CC"), // Blue Bell
		},

		BarLeft:   "█",
		BarRight:  "▐",
		BarTop:    "▄",
		BarBottom: "▀",

		// Geometric rather than typographic, so the four states differ in shape
		// as well as in color.
		GlyphOK:      "●",
		GlyphWarn:    "◆",
		GlyphErr:     "■",
		GlyphLoading: "○",

		Separator: "  ▪  ",

		UpperLabels: true,
		TagTitles:   true,
	}
}

// ANSITheme draws only from the terminal's own sixteen colors, so the
// dashboard inherits whatever scheme the terminal is configured with instead of
// imposing one. Use it when a fixed palette fights your color scheme, or on a
// terminal that has no 256-color support at all.
func ANSITheme() Theme {
	t := DefaultTheme()
	t.Name = "ansi"
	t.Description = "the terminal's own sixteen colors"

	t.OK = lipgloss.Color("2")
	t.Warn = lipgloss.Color("3")
	t.Err = lipgloss.Color("1")
	t.Muted = lipgloss.Color("8")
	t.Accent = lipgloss.Color("5")
	t.Header = lipgloss.Color("7")

	t.Border = lipgloss.Color("8")
	t.Focus = lipgloss.Color("6")
	t.Title = lipgloss.Color("7")
	t.TitleDim = lipgloss.Color("8")
	t.HeaderBG = lipgloss.Color("0")
	t.HeaderFG = lipgloss.Color("7")
	return t
}

// NcursesTheme is the look of a DOS-era curses application — dialog, mc,
// menuconfig, anything built on Turbo Vision: blue panels boxed in double lines,
// white ink, and bright primaries for the things that matter.
//
// It is the first theme with a ground of its own, and that is the whole point of
// it. One departure from the article, for the same reason the LCARS theme has
// its own: the panels are blue rather than the light grey a dialog box wears on
// a blue backdrop. sextant's tiles abut — the gutters are inside the frames — so
// there is almost no backdrop left to be blue, and a screen of grey panels edge
// to edge loses the thing that made the look recognizable. Putting the blue in
// the panels keeps it, and bright-on-blue is the more legible half of the
// tradition anyway.
//
// One blue grounds everything, header band and backdrop included: the header is
// chrome belonging to the same surface, not a separate application, and what
// already separates it from the panes is that the panes have borders and it does
// not. This is also what a dialog backtitle looks like — text laid straight on
// the backdrop, no band.
func NcursesTheme() Theme {
	return Theme{
		Name:        "ncurses",
		Description: "DOS-era curses: blue panels, double-line boxes, white ink",

		// The IBM CGA sixteen, which is what these interfaces actually had.
		// Bright primaries, because everything sits on blue.
		OK:     lipgloss.Color("#55FF55"),
		Warn:   lipgloss.Color("#FFFF55"),
		Err:    lipgloss.Color("#FF5555"),
		Muted:  lipgloss.Color("#AAAAAA"),
		Accent: lipgloss.Color("#55FFFF"),
		Header: lipgloss.Color("#FFFFFF"),

		Border:   lipgloss.Color("#AAAAAA"),
		Focus:    lipgloss.Color("#FFFF55"),
		Title:    lipgloss.Color("#FFFFFF"),
		TitleDim: lipgloss.Color("#AAAAAA"),
		HeaderBG: lipgloss.Color("#0000AA"),
		HeaderFG: lipgloss.Color("#FFFFFF"),

		// No AppBG: the one ground carries the whole screen.
		PaneBG: lipgloss.Color("#0000AA"),
		Text:   lipgloss.Color("#FFFFFF"),

		Frame: FrameRounded,
		Box:   lipgloss.DoubleBorder(),

		// ASCII, as the era had. They also differ in shape, which is what a
		// terminal that has lost its colors needs them to do.
		GlyphOK:      "*",
		GlyphWarn:    "!",
		GlyphErr:     "x",
		GlyphLoading: "?",

		Separator: "  │  ",
	}
}

// themes is the catalog, in the order --list-themes reports.
var themes = []Theme{DefaultTheme(), LCARSTheme(), ANSITheme(), NcursesTheme()}

// current is the applied theme. It is written by [ApplyTheme] and read while
// rendering; see that function's note on which goroutine may call it.
var current = DefaultTheme()

func init() {
	// The Color* and Style* variables have no initialisers of their own, so
	// this is what gives them their values. Go finishes initializing an
	// imported package before the importer starts, so a caller cannot observe
	// them unset.
	ApplyTheme(DefaultTheme())
}

// CurrentTheme returns the applied theme.
func CurrentTheme() Theme { return current }

// ApplyTheme installs t as the active theme, rewriting the package-level
// palette, styles and glyphs that panes and plugins draw with.
//
// Those variables are not synchronized. Call this from startup, or from the
// goroutine that runs the interface's update loop — Bubble Tea serializes
// updates against rendering, so a theme switched there takes effect on the next
// frame with no torn output. Do not call it from a watcher.
func ApplyTheme(t Theme) {
	current = t

	ColorOK, ColorWarn, ColorErr = t.OK, t.Warn, t.Err
	ColorMuted, ColorAccent, ColorHeader = t.Muted, t.Accent, t.Header

	// On a grounded theme every body style carries the pane's ground, not just
	// its own color. A span that sets a foreground alone ends with a reset,
	// which clears the enclosing background too and lipgloss does not re-arm it
	// — so a colored cell would punch a hole through the panel it sits on. This
	// is the same rule the header bar's styles follow, for the same reason.
	fg := func(c lipgloss.Color) lipgloss.Style {
		s := lipgloss.NewStyle().Foreground(c)
		if t.Grounded() {
			s = s.Background(t.PaneBG)
		}
		return s
	}
	StyleOK, StyleWarn, StyleErr, StyleMuted = fg(t.OK), fg(t.Warn), fg(t.Err), fg(t.Muted)
	StyleAccent = fg(t.Accent).Bold(true)
	StyleHeader = fg(t.Header).Bold(true)

	GlyphOK, GlyphWarn = t.GlyphOK, t.GlyphWarn
	GlyphErr, GlyphLoading = t.GlyphErr, t.GlyphLoading
}

// LookupTheme resolves a theme by name. The empty name selects the default.
//
// An unknown name is an error naming the alternatives rather than a silent
// fallback: someone who misspells a theme should be told, not handed the
// default and left wondering why their flag did nothing.
func LookupTheme(name string) (Theme, error) {
	if name == "" {
		return DefaultTheme(), nil
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for _, t := range themes {
		if t.Name == want {
			return t, nil
		}
	}
	return Theme{}, fmt.Errorf("unknown theme %q; available: %s", name, strings.Join(ThemeNames(), ", "))
}

// Themes returns the catalog, in listing order.
func Themes() []Theme { return append([]Theme(nil), themes...) }

// ThemeNames returns every theme name, in listing order.
func ThemeNames() []string {
	out := make([]string, 0, len(themes))
	for _, t := range themes {
		out = append(out, t.Name)
	}
	return out
}

// NextTheme returns the theme after name in the catalog, wrapping at the end.
// An unknown name yields the first, so cycling always lands somewhere valid.
func NextTheme(name string) Theme {
	for i, t := range themes {
		if t.Name == name {
			return themes[(i+1)%len(themes)]
		}
	}
	return themes[0]
}

// MinorSeparator joins fields inside one footer segment, where [Theme.Separator]
// joins the segments themselves.
//
// It is the separator's own glyph with single spaces instead of its padding, so
// the footer keeps two levels of grouping — "Ultra · 4col · 11 panes" reads as
// one thing next to the theme name and the key hints — without a theme having to
// supply a second glyph and keep the two in agreement.
func (t Theme) MinorSeparator() string {
	if glyph := strings.TrimSpace(t.Separator); glyph != "" {
		return " " + glyph + " "
	}
	return " "
}

// Grounded reports whether the theme paints its own backgrounds.
//
// It is keyed on PaneBG alone. A theme that grounds its panes is committing to
// supplying ink as well, and one that does not must be left entirely alone — a
// half-grounded theme is worse than either, because the unpainted cells read as
// damage rather than as a choice.
func (t Theme) Grounded() bool { return t.PaneBG != "" }

// ScreenBG is the ground for the screen outside the panes: the strip beside a
// row of tiles that does not fill the terminal, the rows below the last one, and
// the footer.
//
// It falls back to the pane ground, so one color grounds the whole screen and a
// theme has to opt in to a second.
func (t Theme) ScreenBG() lipgloss.Color {
	if t.AppBG != "" {
		return t.AppBG
	}
	return t.PaneBG
}

// BoxBorder returns the glyphs a [FrameRounded] frame is drawn with.
func (t Theme) BoxBorder() lipgloss.Border {
	if t.Box.Top == "" {
		return lipgloss.RoundedBorder()
	}
	return t.Box
}

// FrameColor picks the frame color for the pane at index in focus order.
//
// index must be stable for a given pane. A theme with no accents gives every
// resting pane the same border, which is the conventional look; one with
// accents spreads them across the panes so neighbors differ.
func (t Theme) FrameColor(index int, focused bool) lipgloss.Color {
	if focused {
		return t.Focus
	}
	if len(t.Accents) == 0 {
		return t.Border
	}
	i := index % len(t.Accents)
	if i < 0 {
		i += len(t.Accents)
	}
	return t.Accents[i]
}

// Label prepares a piece of chrome text — a field label, a pane title, the
// tool's own name — for display. Never pass data through it; see
// [Theme.UpperLabels].
func (t Theme) Label(s string) string {
	if t.UpperLabels {
		return strings.ToUpper(s)
	}
	return s
}

// PaneTitle prepares a pane's title, given the pane's stable ID.
func (t Theme) PaneTitle(id, title string) string {
	out := t.Label(title)
	if t.TagTitles {
		out += " " + paneTag(id)
	}
	return out
}

// paneTag builds an LCARS-style catalog number from a pane ID: two digits, a
// dash, three digits. Hashed rather than random so a pane wears the same number
// every run — a label that changed between launches would read as data.
func paneTag(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	sum := h.Sum32()
	return fmt.Sprintf("%02d-%03d", 10+sum%90, 100+(sum>>8)%900)
}
