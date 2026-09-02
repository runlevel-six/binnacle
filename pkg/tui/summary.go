package tui

// SummaryBlock is a titled group of lines contributed to the overview pane.
//
// It exists so that a subsystem can put its headline where the eye starts without
// the overview knowing what the subsystem is. The overview is core; Ceph, Cilium
// and OpenStack are plugins, and core must not import them — so a plugin hands
// over rendered lines and core decides where they go. A cluster without that
// subsystem contributes nothing, which is how the CAPI-and-Metal3-first shape
// survives: the slot exists, and on most clusters it stays empty.
//
// Two rules the overview enforces on whatever it is given, because a contributed
// block must not be able to disturb the layout above every other pane:
//
// A block gets a column of its own or it does not appear. Stacking it under an
// existing block would make the row taller, and the overview is a fixed-height
// row at the top of the grid — growing it pushes everything else down, at the
// exact moment a subsystem has started misbehaving.
//
// A block is trimmed to the height of the tallest core block, for the same
// reason. A plugin cannot lengthen the row by sending more lines.
type SummaryBlock struct {
	// Title labels the block, in the same style as the overview's own.
	Title string
	// Lines are the body, already formatted. Keep to three: see the height rule.
	Lines []string
}
