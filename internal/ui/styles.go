package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/runlevel-six/sextant/pkg/tui"
)

// ansiReset is the sequence lipgloss ends every styled span with. It clears the
// background as well as the foreground, which is why groundLine exists.
const ansiReset = "\x1b[0m"

// Chrome styles. Pane *body* colors live in pkg/tui so that panes and plugins
// share one palette; these are only for the frame around them.
//
// They are functions rather than variables because the theme can change while
// the program is running: a cached style would keep drawing the old scheme
// until restart. Rebuilding a lipgloss style is cheap next to rendering the
// frame it decorates.
func styleTitle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(tui.CurrentTheme().Title).Bold(true)
}

func styleTitleDim() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(tui.CurrentTheme().TitleDim)
}

func styleStatusBar() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(tui.CurrentTheme().TitleDim)
}

// groundLine lays content on exactly width cells of ground, in ink fg on
// background bg.
//
// It is not a padded Render, because two things defeat one. A style that sets
// only a foreground ends its span with a reset that clears the enclosing
// background as well, and lipgloss does not re-arm it — so the ground is armed
// again after every reset in the line, not only at its start. And body text
// carrying no semantic color is written bare by design (see [tui.HasStyle]), so
// the ground has to supply the ink too.
//
// A theme with no ground gets [padToWidth] back, byte for byte. So does any
// terminal whose color profile cannot express the ground, which is what keeps a
// monochrome terminal from being handed sequences it will print as text.
func groundLine(content string, width int, fg, bg lipgloss.Color) string {
	return rearm(padToWidth(content, width), groundSeq(fg, bg))
}

// rearm applies arm to s and again after every reset inside it.
//
// Use it wherever a run of chrome is assembled from spans somebody else styled —
// a pane title carrying its jump digit, say. The nested span's reset would
// otherwise leave everything after it in the run unpainted. An empty arm returns
// s untouched, which is the ungrounded case.
func rearm(s, arm string) string {
	if arm == "" {
		return s
	}
	return arm + strings.ReplaceAll(s, ansiReset, ansiReset+arm) + ansiReset
}

// groundSeq builds the SGR sequence that arms fg and bg, or "" if neither
// survives the active color profile.
//
// The profile is consulted rather than assumed: lipgloss degrades a hex color
// to the 256-cube or the basic sixteen depending on what the terminal claims,
// and a ground written in truecolor to a terminal that cannot do it would come
// out as garbage in the middle of the frame.
func groundSeq(fg, bg lipgloss.Color) string {
	p := lipgloss.ColorProfile()
	// An unparseable or profile-less color comes back as a nil Color, so this
	// cannot go straight to Sequence.
	seq := func(c lipgloss.Color, background bool) string {
		if c == "" {
			return ""
		}
		resolved := p.Color(string(c))
		if resolved == nil {
			return ""
		}
		return resolved.Sequence(background)
	}

	var parts []string
	for _, s := range []string{seq(fg, false), seq(bg, true)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// backdrop draws width cells of the screen ground: the strip beside a row of
// tiles too narrow to fill the terminal, and the rows below the last one.
func backdrop(width int) string {
	th := tui.CurrentTheme()
	return groundLine("", width, th.Text, th.ScreenBG())
}

// paneBox is one pane's frame request.
type paneBox struct {
	// Title is drawn into the top edge, already themed and carrying its jump
	// digit.
	Title string
	// Width and Height are the exact outer dimensions the frame must occupy.
	Width, Height int
	Focused       bool
	// Index is the pane's position in focus order. A theme that spreads accent
	// colors across panes selects this pane's from it, and the block frame
	// seeds its asymmetry with it, so it must be stable for a given pane.
	Index int
	// Body is the pane's rendered content, already sized to the frame's inner
	// area. It is clipped, never wrapped.
	Body string
}

// paneFrame draws a bordered box of exactly Width x Height with the title
// spliced into the top edge and the body inside.
//
// The body is clipped, never wrapped. Wrapping would silently change a pane's
// line count and push the grid off the bottom of the terminal, so a pane that
// overflows loses content rather than the layout losing integrity.
func paneFrame(b paneBox) string {
	if b.Width < 4 || b.Height < 3 {
		// Too small for a usable border; hand back the clipped body.
		return clipBlock(b.Body, b.Width, b.Height)
	}

	th := tui.CurrentTheme()
	color := th.FrameColor(b.Index, b.Focused)
	bodyW := max(b.Width-tui.PaneChromeH, 1)
	innerH := b.Height - tui.PaneChromeV

	// One blank line above the content when there is slack, mirroring the
	// one-cell horizontal gutter. A pane whose content really does reach the
	// bottom of the frame keeps its flush top.
	body := b.Body
	if contentHeight(body) < innerH {
		body = "\n" + body
	}

	if th.Frame == tui.FrameBlock {
		return blockFrame(b, th, color, body, bodyW, innerH)
	}
	return borderFrame(b, th, color, body, bodyW, innerH)
}

// borderFrame draws the conventional box-drawing frame.
func borderFrame(b paneBox, th tui.Theme, color lipgloss.Color, body string, bodyW, innerH int) string {
	border := th.BoxBorder()
	style := lipgloss.NewStyle().Border(border).BorderForeground(color)

	// lipgloss counts Width as inclusive of padding but exclusive of border,
	// so bodyW+2 leaves exactly bodyW columns of content between the paddings.
	// Without the offset, a line exactly bodyW wide wraps and the pane grows a
	// row — which is how a grid ends up taller than the terminal. Bottom
	// padding falls out of Height() filling the underflow.
	content := clipBlock(body, bodyW, innerH)
	if th.Grounded() {
		// The interior is grounded here rather than by lipgloss. Its padding and
		// height fill would be painted, but only as far as the first reset inside
		// a body line — see groundLine — so the horizontal padding is folded into
		// each line and the block is grown to innerH, leaving lipgloss nothing to
		// fill and nothing to get wrong.
		style = style.BorderBackground(th.PaneBG)
		content = groundBlock(content, bodyW+2, innerH, th)
	} else {
		style = style.Padding(0, 1)
	}

	rendered := style.Width(bodyW + 2).Height(innerH).Render(content)

	lines := strings.Split(rendered, "\n")
	if len(lines) > 0 {
		lines[0] = topBorderWithTitle(b.Width, b.Title, color, border, b.Focused, th)
	}
	return strings.Join(lines, "\n")
}

// contentHeight is how far down the frame a body's content actually reaches:
// its line count, less the blank lines at its foot.
//
// Not [lipgloss.Height], because half the panes on screen pad their own body out
// to the rectangle they were handed — every merged frame does, since a section
// that came up short must not let the next one slide up — and a body of ten lines
// and twenty blanks has the same height as one that fills the tile. Measuring the
// content instead is what tells the two apart, and the difference is exactly the
// slack the top gutter is spent from.
//
// Blankness is judged after the escape sequences are stripped: on a grounded theme
// an empty line is a run of painted spaces and a reset, not an empty string.
func contentHeight(body string) int {
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
}

// groundBlock grounds a body block: every line laid on exactly width cells,
// padded out to exactly height lines so the caller's frame has no gaps to fill.
func groundBlock(body string, width, height int, th tui.Theme) string {
	lines := strings.Split(body, "\n")
	out := make([]string, height)
	for i := range height {
		content := ""
		if i < len(lines) {
			content = lines[i]
		}
		// The one-cell gutter each side goes inside the ground, or the panel
		// would be drawn with two unpainted columns down it.
		out[i] = groundLine(" "+content+" ", width, th.Text, th.PaneBG)
	}
	return strings.Join(out, "\n")
}

// topBorderWithTitle rebuilds the top border line with the title inlaid.
func topBorderWithTitle(width int, title string, color lipgloss.Color, border lipgloss.Border, focused bool, th tui.Theme) string {
	const leftDashes = 1
	titleSeg := " " + title + " "
	available := max(width-2-leftDashes, 0)
	if lipgloss.Width(titleSeg) > available {
		titleSeg = truncate(titleSeg, available)
	}
	rightDashes := max(width-2-leftDashes-lipgloss.Width(titleSeg), 0)

	ink := th.TitleDim
	borderStyle := lipgloss.NewStyle().Foreground(color)
	titleStyle := styleTitleDim()
	if focused {
		ink, titleStyle = th.Title, styleTitle()
	}
	// Both spans carry the ground, since each ends with a reset that would
	// otherwise leave a notch in the top edge where the title sits. The title is
	// re-armed as well as grounded: it arrives already carrying its jump digit in
	// a style of its own, whose reset would strand the rest of the segment.
	titleRun := titleStyle.Render(titleSeg)
	if th.Grounded() {
		borderStyle = borderStyle.Background(th.PaneBG)
		titleRun = rearm(titleStyle.Background(th.PaneBG).Render(titleSeg),
			groundSeq(ink, th.PaneBG))
	}
	return borderStyle.Render(border.TopLeft+strings.Repeat(border.Top, leftDashes)) +
		titleRun +
		borderStyle.Render(strings.Repeat(border.Top, rightDashes)+border.TopRight)
}

// blockFrame draws the solid-bar frame: a heavy rail down each side, and
// half-block bars that hug the content above and below.
//
// It spends exactly the same chrome budget as [borderFrame] — two columns and
// two rows, per [tui.PaneChromeH] and [tui.PaneChromeV] — because the layout
// has already sized the tile on that promise. It cannot ask for the asymmetric
// single-sided elbow the real article has for the same reason.
func blockFrame(b paneBox, th tui.Theme, color lipgloss.Color, body string, bodyW, innerH int) string {
	rail := lipgloss.NewStyle().Foreground(color)
	if th.Grounded() {
		rail = rail.Background(th.PaneBG)
	}
	left, right := rail.Render(th.BarLeft), rail.Render(th.BarRight)
	// The gutter beside each rail is drawn through the rail's style on a grounded
	// theme so it is painted, and left as a bare space otherwise so an ungrounded
	// frame is unchanged.
	gutter := " "
	if th.Grounded() {
		gutter = rail.Render(" ")
	}

	lines := make([]string, 0, b.Height)
	lines = append(lines, blockTopBar(b, th, color, th.BarTop))

	bodyLines := strings.Split(clipBlock(body, bodyW, innerH), "\n")
	for i := range innerH {
		content := ""
		if i < len(bodyLines) {
			content = bodyLines[i]
		}
		lines = append(lines, left+gutter+groundLine(content, bodyW, th.Text, th.PaneBG)+gutter+right)
	}

	lines = append(lines, blockBottomBar(b.Width, color, th.BarBottom, b.Index, th))
	return strings.Join(lines, "\n")
}

// blockTopBar draws the top bar with the title sitting in a gap near its left
// end, which is where an LCARS panel carries its label.
func blockTopBar(b paneBox, th tui.Theme, color lipgloss.Color, glyph string) string {
	const leftSegment = 2
	titleSeg := " " + b.Title + " "
	available := max(b.Width-leftSegment, 0)
	if lipgloss.Width(titleSeg) > available {
		titleSeg = truncate(titleSeg, available)
	}
	rightSegment := max(b.Width-leftSegment-lipgloss.Width(titleSeg), 0)

	ink := th.TitleDim
	barStyle := lipgloss.NewStyle().Foreground(color)
	titleStyle := styleTitleDim()
	if b.Focused {
		ink, titleStyle = th.Title, styleTitle()
	}
	// As in topBorderWithTitle: grounded, and re-armed because the title carries
	// a jump digit styled by the caller.
	titleRun := titleStyle.Render(titleSeg)
	if th.Grounded() {
		barStyle = barStyle.Background(th.PaneBG)
		titleRun = rearm(titleStyle.Background(th.PaneBG).Render(titleSeg),
			groundSeq(ink, th.PaneBG))
	}
	return barStyle.Render(strings.Repeat(glyph, leftSegment)) +
		titleRun +
		segmentedBar(rightSegment, tailLength(b.Index), barStyle, glyph, th)
}

// blockBottomBar draws the bottom bar as two uneven segments.
func blockBottomBar(width int, color lipgloss.Color, glyph string, index int, th tui.Theme) string {
	style := lipgloss.NewStyle().Foreground(color)
	if th.Grounded() {
		style = style.Background(th.PaneBG)
	}
	// A third of the bar, give or take a slice of a third: enough variation
	// between panes to look divided on purpose, never so much that one segment
	// shrinks to nothing.
	third := max(width/3, 1)
	return segmentedBar(width, width-third-vary(index, 7, third), style, glyph, th)
}

// segmentedBar draws a run of exactly width cells, broken once so that tail
// cells sit past the break.
//
// The break is what stops a screen of these reading as a plain table of boxes:
// LCARS chrome is divided at points that look arbitrary, and a bar that always
// split down the middle would look like a mistake instead. Callers derive tail
// from the pane's index rather than from anything varying, so a pane's chrome is
// identical frame to frame.
//
// A width with no room for two recognizable segments is drawn solid. Better a
// plain bar than one broken into fragments too short to read as segments at all.
func segmentedBar(width, tail int, style lipgloss.Style, glyph string, th tui.Theme) string {
	const gap = 2
	const minSegment = 4
	head := width - tail - gap
	if head < minSegment || tail < minSegment {
		return style.Render(strings.Repeat(glyph, max(width, 0)))
	}
	// The break in the bar is a gap in the glyphs, not a gap in the panel: on a
	// grounded theme it is still painted, or every bar would be interrupted by
	// two cells of bare terminal.
	gapFill := strings.Repeat(" ", gap)
	if th.Grounded() {
		gapFill = groundLine(gapFill, gap, th.Text, th.PaneBG)
	}
	return style.Render(strings.Repeat(glyph, head)) +
		gapFill +
		style.Render(strings.Repeat(glyph, tail))
}

// tailLength picks the length of a top bar's trailing segment, varied by pane so
// that neighboring panes are not divided at the same point.
func tailLength(index int) int {
	return 4 + vary(index, 3, 5)
}

// vary spreads a pane index over [0, mod) with the given stride. It is defined
// for a negative index too — a caller has no business passing one, but a frame
// that came out geometrically invalid because of it would be a strange way to
// find out.
func vary(index, stride, mod int) int {
	if mod <= 0 {
		return 0
	}
	v := (index * stride) % mod
	if v < 0 {
		v += mod
	}
	return v
}

// clipBlock truncates body to width columns and height rows.
//
// Unlike a lipgloss Width/Height style it does not pad short lines, which keeps
// a pane's own coloring from being extended across trailing whitespace.
func clipBlock(body string, width, height int) string {
	lines := strings.Split(body, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = truncate(ln, width)
		}
	}
	return strings.Join(lines, "\n")
}

// truncate cuts s to maxWidth display columns, preserving escape sequences.
//
// Cutting runes naively can land inside an escape sequence; the fragment left
// behind then counts as printable text and the "truncated" line ends up wider
// than the limit.
func truncate(s string, maxWidth int) string {
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	return ansi.Truncate(s, maxWidth, "")
}

// padToWidth right-pads a single line to exactly width columns.
//
// Bubble Tea only repaints cells it is given, so a short line leaves whatever
// was on screen before it — this is what stops a previous frame's chrome
// bleeding through.
func padToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	switch {
	case w < width:
		return s + strings.Repeat(" ", width-w)
	case w > width:
		return truncate(s, width)
	}
	return s
}
