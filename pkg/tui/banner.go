package tui

import "github.com/charmbracelet/lipgloss"

// BannerStatus is one subsystem's health, ordered by severity so that the
// worst status wins when several are aggregated into one cell.
type BannerStatus int

const (
	// BannerLoading means the underlying data has not arrived yet. It is the
	// zero value, so a cell built before its source has published reads as
	// "unknown" rather than as healthy.
	BannerLoading BannerStatus = iota
	// BannerOK means the subsystem is healthy.
	BannerOK
	// BannerWarn means degraded but functioning.
	BannerWarn
	// BannerErr means broken.
	BannerErr
)

// String returns the status name, for diagnostics.
func (s BannerStatus) String() string {
	switch s {
	case BannerLoading:
		return "loading"
	case BannerOK:
		return "ok"
	case BannerWarn:
		return "warn"
	case BannerErr:
		return "err"
	}
	return "?"
}

// Glyph returns the indicator character and style for a status, both from the
// active theme.
func (s BannerStatus) Glyph() (string, lipgloss.Style) {
	switch s {
	case BannerOK:
		return GlyphOK, StyleOK
	case BannerWarn:
		return GlyphWarn, StyleWarn
	case BannerErr:
		return GlyphErr, StyleErr
	}
	return GlyphLoading, StyleMuted
}

// Worse returns whichever of s and other is more severe. Use it to fold
// several readings into a single cell.
func (s BannerStatus) Worse(other BannerStatus) BannerStatus {
	if other > s {
		return other
	}
	return s
}

// BannerCell is one labeled subsystem indicator in the health strip at the
// top of the dashboard.
//
// Detail should be set only when the subsystem is degraded. A healthy cell
// renders as just its name and glyph, which is what keeps the strip scannable
// when everything is fine — if every cell always carried detail, the row would
// be a wall of text and the one broken subsystem would not stand out.
type BannerCell struct {
	Name   string
	Status BannerStatus
	Detail string
}

// Render draws one cell. nameStyle is supplied by the program so the cell
// blends with whatever background the strip uses.
//
// The status glyph and detail inherit nameStyle's background rather than only
// its own foreground. A style that sets no background emits a reset that clears
// the strip's, which leaves a notch behind every cell — and since the glyph is
// the part a reader is scanning for, that is exactly the wrong place for one.
func (c BannerCell) Render(nameStyle lipgloss.Style) string {
	glyph, style := c.Status.Glyph()
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
