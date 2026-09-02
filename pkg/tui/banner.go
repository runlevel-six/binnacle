package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/pkg/health"
)

// BannerStatus is one subsystem's health, ordered by severity so that the
// worst status wins when several are aggregated into one cell.
//
// It is an alias for [health.Status]: the verdict is decided in pkg/health,
// which knows nothing about terminals, and this package only draws it.
type BannerStatus = health.Status

// The banner statuses, in increasing severity. See [health.Status].
const (
	BannerLoading = health.StatusLoading
	BannerOK      = health.StatusOK
	BannerWarn    = health.StatusWarn
	BannerErr     = health.StatusErr
)

// BannerCell is one labeled subsystem indicator in the health strip at the top
// of the dashboard. It is an alias for [health.Cell].
type BannerCell = health.Cell

// Glyph returns the indicator character and style for a status, both from the
// active theme.
func Glyph(s BannerStatus) (string, lipgloss.Style) {
	switch s {
	case health.StatusOK:
		return GlyphOK, StyleOK
	case health.StatusWarn:
		return GlyphWarn, StyleWarn
	case health.StatusErr:
		return GlyphErr, StyleErr
	}
	return GlyphLoading, StyleMuted
}

// RenderCell draws one cell. nameStyle is supplied by the program so the cell
// blends with whatever background the strip uses.
//
// The status glyph and detail inherit nameStyle's background rather than only
// its own foreground. A style that sets no background emits a reset that clears
// the strip's, which leaves a notch behind every cell — and since the glyph is
// the part a reader is scanning for, that is exactly the wrong place for one.
func RenderCell(c BannerCell, nameStyle lipgloss.Style) string {
	glyph, style := Glyph(c.Status)
	if bg := nameStyle.GetBackground(); bg != nil {
		if _, unset := bg.(lipgloss.NoColor); !unset {
			style = style.Background(bg)
		}
	}
	out := nameStyle.Render(c.Name) + nameStyle.Render(" ") + style.Render(glyph)
	if c.Detail != "" {
		out += nameStyle.Render(" ") + style.Render(c.Detail)
	}
	return out
}
