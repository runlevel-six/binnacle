package table

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/binnacle/pkg/tui"
)

// TestMain forces a color-capable profile.
//
// lipgloss detects terminal capability at init, and under `go test` there is no
// TTY, so the profile degrades to Ascii and every Render call strips styling.
// Any test comparing styled output against unstyled would then pass vacuously —
// both sides are plain text. Forcing TrueColor makes those assertions real.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// lines splits rendered output for per-line assertions.
func lines(s string) []string { return strings.Split(s, "\n") }

func cols(headers ...string) []Column {
	out := make([]Column, 0, len(headers))
	for _, h := range headers {
		out = append(out, Column{Header: h})
	}
	return out
}

// --- PadOrTrunc -----------------------------------------------------------

func TestPadOrTrunc(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{"abc", 5, "abc  "},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc"},
		{"", 3, "   "},
		{"abc", 0, ""},
		{"abc", -1, ""},
	}
	for _, tc := range tests {
		if got := PadOrTrunc(tc.in, tc.width); got != tc.want {
			t.Errorf("PadOrTrunc(%q, %d): got %q want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

// PadOrTrunc measures display width, so a wide rune counts as two cells.
func TestPadOrTrunc_WideRunes(t *testing.T) {
	// "世界" is 4 display cells wide across 2 runes.
	if got := lipgloss.Width(PadOrTrunc("世界", 4)); got != 4 {
		t.Errorf("width of padded wide string: got %d want 4", got)
	}
	// Truncating to 3 can only fit one wide rune, leaving width 2.
	if got := lipgloss.Width(PadOrTrunc("世界", 3)); got > 3 {
		t.Errorf("truncated wide string overflows: got width %d want <= 3", got)
	}
}

// --- ClipLines ------------------------------------------------------------

func TestClipLines(t *testing.T) {
	in := "a\nb\nc"
	if got := ClipLines(in, 2); got != "a\nb" {
		t.Errorf("ClipLines: got %q want %q", got, "a\nb")
	}
	if got := ClipLines(in, 10); got != in {
		t.Errorf("ClipLines beyond length: got %q want %q", got, in)
	}
	if got := ClipLines(in, 0); got != "" {
		t.Errorf("ClipLines(0): got %q want empty", got)
	}
}

// --- layout ---------------------------------------------------------------

func TestLayoutInner_ExactFit(t *testing.T) {
	c := []Column{{Header: "NAME", Width: 10}, {Header: "AGE", Width: 5}}
	// 10 + 5 + one MinGap = 17
	widths, gaps := LayoutInner(c, nil, 17)
	if widths[0] != 10 || widths[1] != 5 {
		t.Errorf("widths: got %v want [10 5]", widths)
	}
	if len(gaps) != 1 || gaps[0] != MinGap {
		t.Errorf("gaps: got %v want [%d]", gaps, MinGap)
	}
}

func TestLayoutInner_AutoSizesToWidestCell(t *testing.T) {
	c := cols("NAME", "STATUS")
	rows := [][]string{{"short", "Ready"}, {"a-much-longer-name", "NotReady"}}
	widths, _ := LayoutInner(c, rows, 200)
	if widths[0] != len("a-much-longer-name") {
		t.Errorf("col 0 width: got %d want %d", widths[0], len("a-much-longer-name"))
	}
	if widths[1] != len("NotReady") {
		t.Errorf("col 1 width: got %d want %d", widths[1], len("NotReady"))
	}
}

func TestLayoutInner_HeaderIsAWidthFloor(t *testing.T) {
	c := cols("VERY-LONG-HEADER")
	widths, _ := LayoutInner(c, [][]string{{"x"}}, 100)
	if widths[0] != len("VERY-LONG-HEADER") {
		t.Errorf("width: got %d want %d (header floor)", widths[0], len("VERY-LONG-HEADER"))
	}
}

func TestLayoutInner_StretchAbsorbsSlackUpToCap(t *testing.T) {
	c := []Column{{Header: "NAME"}, {Header: "REASON", Stretch: true}}
	rows := [][]string{{"node-1", "Evicted"}}
	// Natural: 6 + 7 + 2 gap = 15. Plenty of slack at 200.
	widths, _ := LayoutInner(c, rows, 200)
	natural := len("Evicted") // wider than the "REASON" header
	if got, want := widths[1], natural+MaxStretchPad; got != want {
		t.Errorf("stretch width: got %d want %d (natural+cap)", got, want)
	}
	// Slack past the cap must not inflate the non-stretch column.
	if widths[0] != len("node-1") {
		t.Errorf("non-stretch column grew: got %d want %d", widths[0], len("node-1"))
	}
}

// A stretch column with no content is demoted, so it doesn't absorb slack into
// an invisible right margin.
func TestLayoutInner_EmptyStretchColumnIsDemoted(t *testing.T) {
	c := []Column{{Header: "NAME"}, {Header: "NOTE", Stretch: true}}
	rows := [][]string{{"node-1", ""}, {"node-2", "   "}}
	widths, _ := LayoutInner(c, rows, 200)
	if widths[1] != len("NOTE") {
		t.Errorf("demoted stretch column: got width %d want %d", widths[1], len("NOTE"))
	}
}

func TestLayoutInner_OverflowShrinksStretchFirst(t *testing.T) {
	c := []Column{{Header: "NAME"}, {Header: "MSG", Stretch: true}}
	rows := [][]string{{"node-1", "a-very-long-message-here"}}
	// Natural is 6 + 24 + 2 = 32; squeeze to 20.
	widths, _ := LayoutInner(c, rows, 20)
	if widths[0] != len("node-1") {
		t.Errorf("non-stretch column should be untouched first: got %d want 6", widths[0])
	}
	if widths[1] > len("a-very-long-message-here") {
		t.Errorf("stretch column should have shrunk, got %d", widths[1])
	}
	if total := widths[0] + widths[1] + MinGap; total > 20 {
		t.Errorf("total %d exceeds innerWidth 20", total)
	}
}

// Once the stretch column is at its header floor, the widest remaining column
// gives up cells — but never below minColWidth.
func TestLayoutInner_OverflowShrinksWidestAndRespectsFloor(t *testing.T) {
	c := cols("AAAAAAAAAA", "BBBBBBBBBB")
	widths, _ := LayoutInner(c, nil, 8)
	for i, w := range widths {
		if w < minColWidth {
			t.Errorf("col %d shrank to %d, below floor %d", i, w, minColWidth)
		}
	}
}

func TestNaturalRowWidth(t *testing.T) {
	c := []Column{{Header: "AB"}, {Header: "CD", Width: 7}}
	rows := [][]string{{"abcd", "x"}}
	// col0 natural 4 ("abcd"), col1 pinned 7, plus one MinGap.
	if got, want := NaturalRowWidth(c, rows), 4+7+MinGap; got != want {
		t.Errorf("NaturalRowWidth: got %d want %d", got, want)
	}
}

func TestNaturalRowWidth_SingleColumnHasNoGap(t *testing.T) {
	if got, want := NaturalRowWidth(cols("ABCDE"), nil), 5; got != want {
		t.Errorf("NaturalRowWidth: got %d want %d", got, want)
	}
}

func TestPaneLeftPad(t *testing.T) {
	narrow := cols("ABCDEFGHIJKLMNOPQRST") // a 20-cell header row
	// No slack once MaxStretchPad is reserved: no pad.
	if got := PaneLeftPad(30, narrow); got != 0 {
		t.Errorf("tight pane: got pad %d want 0", got)
	}
	// Abundant slack is capped.
	if got := PaneLeftPad(400, narrow); got != EdgePadCap {
		t.Errorf("wide pane: got pad %d want %d", got, EdgePadCap)
	}
	// The widest schema governs shared alignment.
	wide := cols("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCD") // 40 cells
	if got, want := PaneLeftPad(60, cols("ABCDEFGHIJ"), wide, narrow),
		min((60-40-MaxStretchPad)/2, EdgePadCap); got != want {
		t.Errorf("multi-table pad: got %d want %d", got, want)
	}
}

// The pad is what keeps a table off the pane border, and it must not be the reason
// the table moves. A single long cell used to push the whole pane four cells left
// and let it back when the cell went away, on whatever timer the data changed on.
func TestPaneLeftPad_UnmovedByContent(t *testing.T) {
	schema := []Column{{Header: "POD", Stretch: true}, {Header: "STATUS"}, {Header: "NODE"}}
	want := PaneLeftPad(120, schema)

	for _, rows := range [][][]string{
		nil,
		{{"a", "Running", "n1"}},
		{{"openstack/nova-compute-controller-manager-5d9c7b8f4d-xk2mn", "CrashLoopBackOff",
			"compute-node-3.site-a.example.com"}},
		{{strings.Repeat("x", 400), "CrashLoopBackOff", strings.Repeat("y", 400)}},
	} {
		if _, _, got := Layout(schema, rows, 120); got != want {
			t.Errorf("pad moved to %d (want %d) for content %d cells wide",
				got, want, NaturalRowWidth(schema, rows))
		}
	}
}

// --- rendering ------------------------------------------------------------

func TestTableRender_HeaderAndRows(t *testing.T) {
	tbl := Table{
		Cols: cols("NAME", "STATUS"),
		Rows: [][]string{{"node-1", "Ready"}, {"node-2", "NotReady"}},
	}
	out := lines(tbl.Render(40, 10))
	if len(out) != 3 {
		t.Fatalf("lines: got %d want 3 (header + 2 rows)\n%q", len(out), out)
	}
	if !strings.Contains(out[0], "NAME") || !strings.Contains(out[0], "STATUS") {
		t.Errorf("header line missing columns: %q", out[0])
	}
	if !strings.Contains(out[1], "node-1") || !strings.Contains(out[2], "node-2") {
		t.Errorf("row content wrong: %q", out[1:])
	}
}

func TestTableRender_OverflowShowsMoreFooter(t *testing.T) {
	rows := make([][]string, 10)
	for i := range rows {
		rows[i] = []string{"node", "Ready"}
	}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	// Width 18 holds the table once and not twice, so the rows cannot flow and
	// height 5 = header + 3 data rows + footer, eliding 7.
	out := lines(tbl.Render(18, 5))
	if len(out) != 5 {
		t.Fatalf("lines: got %d want 5\n%q", len(out), out)
	}
	last := out[len(out)-1]
	if !strings.Contains(last, "+ 7 more") {
		t.Errorf("footer: got %q want to contain %q", last, "+ 7 more")
	}
}

// --- row flow -------------------------------------------------------------

func TestFlowGroups(t *testing.T) {
	c := cols("NAME", "STATUS") // natural width 12: 4 + 2 + 6
	rows := func(n int) [][]string {
		out := make([][]string, n)
		for i := range out {
			out[i] = []string{"node", "Ready"}
		}
		return out
	}

	tests := []struct {
		name          string
		rows          int
		width, height int
		want          int
	}{
		{"rows fit, width to spare", 3, 200, 5, 1},
		{"rows overflow, no width", 20, 18, 5, 1},
		{"rows overflow, room for two", 20, 40, 5, 2},
		{"rows overflow, room for four", 40, 64, 5, 4},
		{"width for five, rows need two", 7, 80, 5, 2},
		{"no rows", 0, 200, 20, 1},
		{"no room for a header", 20, 200, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FlowGroups(c, rows(tc.rows), tc.width, tc.height); got != tc.want {
				t.Errorf("FlowGroups(%d rows, %dx%d): got %d want %d",
					tc.rows, tc.width, tc.height, got, tc.want)
			}
		})
	}
}

// Flowed groups fill downwards and then across, so the head of the list stays in
// the top left where an unflowed table had it.
func TestTableRender_FlowFillsColumnMajor(t *testing.T) {
	rows := [][]string{{"n1", "Ready"}, {"n2", "Ready"}, {"n3", "Ready"}, {"n4", "Ready"}}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	// Height 3 = header + 2 rows per group; width 40 holds two groups.
	out := lines(tbl.Render(40, 3))
	if len(out) != 3 {
		t.Fatalf("lines: got %d want 3\n%q", len(out), out)
	}
	if !strings.Contains(out[1], "n1") || !strings.Contains(out[1], "n3") {
		t.Errorf("first data line should hold n1 and n3: %q", out[1])
	}
	if !strings.Contains(out[2], "n2") || !strings.Contains(out[2], "n4") {
		t.Errorf("second data line should hold n2 and n4: %q", out[2])
	}
	if strings.Count(out[0], "NAME") != 2 {
		t.Errorf("each group repeats the header: %q", out[0])
	}
}

// Every group puts its columns at the same offsets, including the last one, which
// is a cell or two wider because it absorbs the rounding remainder.
func TestTableRender_FlowAlignsGroupColumns(t *testing.T) {
	rows := make([][]string, 6)
	for i := range rows {
		rows[i] = []string{"node", "Ready"}
	}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	out := lines(tbl.Render(41, 4)) // odd width, so the groups cannot be equal
	header := out[0]
	first := strings.Index(header, "STATUS")
	second := strings.Index(header[first+1:], "STATUS") + first + 1
	third := strings.Index(header, "NAME")
	fourth := strings.Index(header[third+1:], "NAME") + third + 1
	if second-first != fourth-third {
		t.Errorf("group column offsets differ: NAME %d apart, STATUS %d apart\n%q",
			fourth-third, second-first, header)
	}
}

// The footer counts every row no group reached, not just the last group's
// leftovers.
func TestTableRender_FlowFooterCountsAllHiddenRows(t *testing.T) {
	rows := make([][]string, 10)
	for i := range rows {
		rows[i] = []string{"node", "Ready"}
	}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	// Two groups of 4 lines: 4 rows in the first, 3 plus a footer in the second.
	out := lines(tbl.Render(40, 5))
	last := out[len(out)-1]
	if !strings.Contains(last, "+ 3 more") {
		t.Errorf("footer: got %q want to contain %q", last, "+ 3 more")
	}
}

// A flowed table still honors its rectangle exactly: same width on every line, no
// trailing group left half-drawn.
func TestTableRender_FlowFillsWidthExactly(t *testing.T) {
	rows := make([][]string, 30)
	for i := range rows {
		rows[i] = []string{"node", "Ready"}
	}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	for _, w := range []int{37, 40, 41, 63, 200} {
		out := lines(tbl.Render(w, 6))
		for i, ln := range out {
			if got := lipgloss.Width(ln); got != w {
				t.Errorf("render(%d,6) line %d: width %d want %d", w, i, got, w)
			}
		}
	}
}

func TestTableRender_NeverExceedsBounds(t *testing.T) {
	rows := make([][]string, 40)
	for i := range rows {
		rows[i] = []string{"a-fairly-long-node-name", "CrashLoopBackOff"}
	}
	tbl := Table{Cols: cols("NAME", "STATUS"), Rows: rows}
	for _, h := range []int{1, 2, 3, 8, 20} {
		for _, w := range []int{10, 25, 60, 200} {
			out := tbl.Render(w, h)
			ls := lines(out)
			if len(ls) > h {
				t.Errorf("render(%d,%d): %d lines exceeds height", w, h, len(ls))
			}
			for i, ln := range ls {
				if got := lipgloss.Width(ln); got > w {
					t.Errorf("render(%d,%d) line %d width %d exceeds width", w, h, i, got)
				}
			}
		}
	}
}

func TestTableRender_ZeroDimensions(t *testing.T) {
	tbl := Table{Cols: cols("A"), Rows: [][]string{{"x"}}}
	if got := tbl.Render(0, 5); got != "" {
		t.Errorf("zero width: got %q want empty", got)
	}
	if got := tbl.Render(5, 0); got != "" {
		t.Errorf("zero height: got %q want empty", got)
	}
}

func TestTableRender_ShortRowsPadBlanks(t *testing.T) {
	tbl := Table{
		Cols: cols("A", "B", "C"),
		Rows: [][]string{{"only-one"}},
	}
	out := lines(tbl.Render(40, 5))
	if len(out) != 2 {
		t.Fatalf("lines: got %d want 2", len(out))
	}
	if !strings.Contains(out[1], "only-one") {
		t.Errorf("row: got %q", out[1])
	}
}

// RowStyles shorter than Rows must not panic — v1 indexed it unconditionally.
func TestTableRender_ShortRowStylesDoNotPanic(t *testing.T) {
	tbl := Table{
		Cols:      cols("A"),
		Rows:      [][]string{{"one"}, {"two"}, {"three"}},
		RowStyles: []lipgloss.Style{tui.StyleOK}, // deliberately too short
	}
	out := tbl.Render(20, 6)
	if !strings.Contains(out, "three") {
		t.Errorf("render dropped rows: %q", out)
	}
}

// Style precedence is asserted on the resolved style rather than on rendered
// bytes, so the test says what it means and does not depend on the active
// color profile.
func TestResolveStyle_Precedence(t *testing.T) {
	col := Column{Header: "A", Style: tui.StyleMuted}
	cellStyles := []lipgloss.Style{tui.StyleErr}

	tests := []struct {
		name       string
		rowStyle   lipgloss.Style
		cellStyles []lipgloss.Style
		isHeader   bool
		want       lipgloss.Style
	}{
		{"header beats everything", tui.StyleOK, cellStyles, true, tui.StyleHeader},
		{"cell beats row", tui.StyleOK, cellStyles, false, tui.StyleErr},
		{"row beats column", tui.StyleOK, nil, false, tui.StyleOK},
		{"column is the fallback", lipgloss.Style{}, nil, false, tui.StyleMuted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStyle(col, tc.rowStyle, tc.cellStyles, 0, tc.isHeader)
			if got.GetForeground() != tc.want.GetForeground() {
				t.Errorf("got fg %v want %v", got.GetForeground(), tc.want.GetForeground())
			}
		})
	}
}

// With no candidate carrying an attribute, the zero style is returned so the
// caller writes the cell bare.
func TestResolveStyle_NoCandidateYieldsUnstyled(t *testing.T) {
	got := resolveStyle(Column{Header: "A"}, lipgloss.Style{}, nil, 0, false)
	if tui.HasStyle(got) {
		t.Errorf("expected an unstyled result, got fg %v", got.GetForeground())
	}
}

// A cellStyles entry that is the zero value must fall through rather than
// suppressing the row style.
func TestResolveStyle_ZeroCellStyleFallsThrough(t *testing.T) {
	got := resolveStyle(Column{Header: "A"}, tui.StyleOK, []lipgloss.Style{{}}, 0, false)
	if got.GetForeground() != tui.StyleOK.GetForeground() {
		t.Errorf("got fg %v want the row style %v", got.GetForeground(), tui.StyleOK.GetForeground())
	}
}

// Column.Style was unreachable in the predecessor implementation because the
// row-style branch always matched. This guards the fix.
func TestRenderRowCells_ColumnStyleIsApplied(t *testing.T) {
	c := []Column{{Header: "A", Style: tui.StyleErr}}
	widths, gaps := LayoutInner(c, [][]string{{"x"}}, 10)

	styled := RenderRowCells(c, widths, gaps, []string{"x"}, lipgloss.Style{}, nil, false)
	bare := RenderRowCells([]Column{{Header: "A"}}, widths, gaps, []string{"x"}, lipgloss.Style{}, nil, false)
	if styled == bare {
		t.Error("Column.Style had no effect on rendered output")
	}
}

func TestRenderRow_UnstyledCellHasNoEscapes(t *testing.T) {
	c := cols("A")
	widths, gaps := LayoutInner(c, [][]string{{"x"}}, 10)
	got := RenderRow(c, widths, gaps, []string{"x"}, lipgloss.Style{}, false)
	if strings.Contains(got, "\x1b") {
		t.Errorf("unstyled cell emitted escape sequences: %q", got)
	}
}

func TestIndentLines(t *testing.T) {
	got := IndentLines("a\n\nb", 2)
	want := "  a\n\n  b" // blank line stays bare
	if got != want {
		t.Errorf("IndentLines: got %q want %q", got, want)
	}
	if got := IndentLines("a", 0); got != "a" {
		t.Errorf("zero pad: got %q want %q", got, "a")
	}
	if got := IndentLines("", 4); got != "" {
		t.Errorf("empty body: got %q want empty", got)
	}
}

// --- empty states ---------------------------------------------------------

func TestPlaceholder_FillsExactHeight(t *testing.T) {
	for _, h := range []int{1, 2, 5, 9} {
		out := lines(Placeholder(30, h, "loading…"))
		if len(out) != h {
			t.Errorf("Placeholder height %d: got %d lines", h, len(out))
		}
	}
	if !strings.Contains(Placeholder(30, 5, "loading…"), "loading…") {
		t.Error("Placeholder dropped its message")
	}
}

func TestErrorBody_FillsExactHeightAndShowsError(t *testing.T) {
	out := ErrorBody(40, 4, errors.New("boom"))
	ls := lines(out)
	if len(ls) != 4 {
		t.Fatalf("lines: got %d want 4", len(ls))
	}
	if !strings.Contains(ls[0], "boom") {
		t.Errorf("first line should carry the error: %q", ls[0])
	}
}

func TestErrorBody_ZeroHeight(t *testing.T) {
	if got := ErrorBody(10, 0, errors.New("x")); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- formatting helpers ---------------------------------------------------

func TestFmtCount(t *testing.T) {
	// Compare against the styled forms rather than asserting escape bytes.
	if got, want := FmtCount(3, 3), tui.StyleOK.Render("3/3"); got != want {
		t.Errorf("equal counts: got %q want %q", got, want)
	}
	if got, want := FmtCount(2, 3), tui.StyleErr.Render("2/3"); got != want {
		t.Errorf("unequal counts: got %q want %q", got, want)
	}
}

func TestShortAge(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{0, "1m"},
		{30, "1m"},
		{59, "1m"},
		{60, "1m"},
		{119, "1m"},
		{120, "2m"},
		{3599, "59m"},
		{3600, "1h"},
		{86399, "23h"},
		{86400, "1d"},
		{86400 * 12, "12d"},
	}
	for _, tc := range tests {
		if got := ShortAge(tc.seconds); got != tc.want {
			t.Errorf("ShortAge(%v): got %q want %q", tc.seconds, got, tc.want)
		}
	}
}

// --- appetite -------------------------------------------------------------

func TestAppetiteWidth(t *testing.T) {
	// A column whose content is written by the situation is charged its header, so
	// a table's declared width does not move when the situation does.
	cols := []Column{
		{Header: "MACHINE"},
		{Header: "HOST STATE", Stretch: true, Transient: true},
	}
	machine := "demo-workers-5d9c7-lm2vp"
	settled := [][]string{{machine, "provisioned"}}
	moving := [][]string{{machine,
		"deprovisioning powered off Introspection timed out after 30m0s"}}

	want := len(machine) + MinGap + len("HOST STATE") + EdgePadCap
	if got := AppetiteWidth(cols, settled); got != want {
		t.Errorf("settled: got %d want %d", got, want)
	}
	if got := AppetiteWidth(cols, moving); got != want {
		t.Errorf("mid-rollout: got %d want %d — a transient cell moved the appetite", got, want)
	}
	// The full natural width still reports the truth, for the layout decisions that
	// want it.
	if NaturalRowWidth(cols, moving) <= NaturalRowWidth(cols, settled) {
		t.Error("NaturalRowWidth should still grow with the content")
	}
}

// A column carrying identity rather than commentary is charged in full, up to the
// cap: a node's FQDN is what a reader picks the row out by.
func TestAppetiteWidth_IdentityColumnsChargedInFull(t *testing.T) {
	cols := []Column{{Header: "NODE", Stretch: true}, {Header: "STATUS"}}
	fqdn := "compute-node-1.site-a.demo.example"
	rows := [][]string{{fqdn, "Ready"}}
	want := len(fqdn) + MinGap + len("STATUS") + EdgePadCap
	if got := AppetiteWidth(cols, rows); got != want {
		t.Errorf("got %d want %d", got, want)
	}
}

// No column is charged more than the cap, however long its content.
func TestAppetiteWidth_CapsAnyOneColumn(t *testing.T) {
	cols := []Column{{Header: "POD", Stretch: true}}
	rows := [][]string{{strings.Repeat("x", 400)}}
	if got, want := AppetiteWidth(cols, rows), StretchAppetiteCap+EdgePadCap; got != want {
		t.Errorf("got %d want %d", got, want)
	}
}
