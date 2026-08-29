// Package health holds sextant's verdicts about a cluster: the small set of
// judgements that decide whether something is fine, degraded, or broken.
//
// It is deliberately free of presentation. A [Cell] is a name, a [Status], and
// an optional detail string — never a glyph, a color, or a rendered line. The
// terminal dashboard draws one way and a web front end draws another, but both
// must reach the *same* conclusion from the same data, because a cluster that
// two of an operator's tools disagree about is a cluster nobody trusts either
// tool on.
//
// The judgements themselves are not obvious, and that is exactly why they live
// in one place. A cordon is expected on a compute node whose capacity is
// reserved and alarming on a control-plane node. A Machine outside Running is
// normal mid-rollout and worth amber, not red. Every one of these was learned
// against a real cluster, and none of them survives being reimplemented from
// the field names alone.
package health

// Status is one subsystem's health, ordered by severity so that the worst wins
// when several readings are folded into one.
type Status int

const (
	// StatusLoading means the underlying data has not arrived yet. It is the
	// zero value, so a cell built before its source has published reads as
	// "unknown" rather than as healthy.
	StatusLoading Status = iota
	// StatusOK means the subsystem is healthy.
	StatusOK
	// StatusWarn means degraded but functioning.
	StatusWarn
	// StatusErr means broken.
	StatusErr
)

// String returns the status name. It is stable, lowercase, and safe to use as
// an identifier in markup or a CSS class.
func (s Status) String() string {
	switch s {
	case StatusLoading:
		return "loading"
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusErr:
		return "err"
	}
	return "?"
}

// Worse returns whichever of s and other is more severe. Use it to fold
// several readings into a single indicator.
func (s Status) Worse(other Status) Status {
	if other > s {
		return other
	}
	return s
}

// Cell is one labeled subsystem indicator.
//
// Detail should be set only when the subsystem is degraded. A healthy cell is
// just its name and status, which is what keeps a strip of them scannable when
// everything is fine — if every cell always carried detail, the row would be a
// wall of text and the one broken subsystem would not stand out.
type Cell struct {
	Name   string
	Status Status
	Detail string
}

// Worst folds a list of cells to the single most severe status.
//
// An empty list is [StatusLoading], not [StatusOK]: nothing has reported, so
// the honest answer is that we do not know. Summarising a cluster we have not
// heard from as healthy is the one mistake a fleet view must not make.
func Worst(cells []Cell) Status {
	worst := StatusLoading
	for _, c := range cells {
		worst = worst.Worse(c.Status)
	}
	return worst
}
