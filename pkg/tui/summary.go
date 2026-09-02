package tui

import "github.com/runlevel-six/binnacle/pkg/plugin"

// SummaryBlock is a titled group of lines contributed to the overview pane.
//
// The type lives in [pkg/plugin] so that [pkg/plugin] does not need to import
// this package. This alias keeps existing callers working: code that references
// tui.SummaryBlock gets plugin.SummaryBlock, which is the same type.
//
// See [plugin.SummaryBlock] for the rules the overview enforces on whatever it
// is handed.
type SummaryBlock = plugin.SummaryBlock
