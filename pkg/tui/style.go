package tui

import "github.com/charmbracelet/lipgloss"

// Semantic colors for pane body content.
//
// These are written by [ApplyTheme] and read when a style is applied, so a
// theme switch reaches every pane and plugin without any of them knowing a
// theme exists. Prefer selecting a theme over assigning these directly; see
// [ApplyTheme] for the goroutine rule.
var (
	ColorOK     lipgloss.Color
	ColorWarn   lipgloss.Color
	ColorErr    lipgloss.Color
	ColorMuted  lipgloss.Color
	ColorAccent lipgloss.Color
	ColorHeader lipgloss.Color
)

// Cell-level styles shared by every pane. Chrome — borders, title bars, status
// lines — is the program's business, not a pane's; everything that goes inside
// a pane body belongs here.
//
// As with the colors above, these are set from the active theme.
var (
	StyleOK     lipgloss.Style
	StyleWarn   lipgloss.Style
	StyleErr    lipgloss.Style
	StyleMuted  lipgloss.Style
	StyleAccent lipgloss.Style
	StyleHeader lipgloss.Style
)

// Status indicator glyphs, likewise theme-supplied. Each must be one cell wide.
var (
	GlyphOK      string
	GlyphWarn    string
	GlyphErr     string
	GlyphLoading string
)

// StatusStyle picks a color for a kubectl-style status string.
//
// Settled-and-healthy states are green, transitional states amber, and failure
// states red. An unrecognized status renders muted rather than guessing, so a
// status this function has never seen reads as "no opinion" instead of as a
// false alarm.
func StatusStyle(s string) lipgloss.Style {
	switch s {
	case "Ready", "Running", "Completed", "Succeeded", "available", "Available":
		return StyleOK
	case "NotReady", "CrashLoopBackOff", "Error", "Failed", "ImagePullBackOff",
		"ErrImagePull", "OOMKilled", "Evicted", "ContainerStatusUnknown":
		return StyleErr
	case "Pending", "ContainerCreating", "PodInitializing", "Terminating",
		"Cordoned", "SchedulingDisabled", "Ready,SchedulingDisabled":
		return StyleWarn
	}
	// Init:0/2, Init:Error, Init:CrashLoopBackOff — all transitional.
	if len(s) >= 5 && s[:5] == "Init:" {
		return StyleWarn
	}
	return StyleMuted
}

// HasStyle reports whether s carries any visible attribute. Used to decide
// between rendering a cell through a style and writing it bare, which keeps
// unstyled output free of empty escape sequences, and to resolve which of
// several candidate styles actually applies.
//
// Note the NoColor check. An unset foreground reads back as
// [lipgloss.NoColor], not as an empty [lipgloss.Color], so the intuitive
// `GetForeground() != lipgloss.Color("")` is true for every style including
// the zero value. Getting this wrong silently collapses style precedence: the
// first candidate always appears to be styled and later ones are never
// consulted.
func HasStyle(s lipgloss.Style) bool {
	if s.GetBold() {
		return true
	}
	fg := s.GetForeground()
	if _, unset := fg.(lipgloss.NoColor); unset {
		return false
	}
	return fg != lipgloss.Color("")
}
