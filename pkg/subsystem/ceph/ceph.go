// Package ceph holds the state sextant's Ceph plugin publishes.
//
// The types are here, separate from the plugin that fills them, so that a
// consumer outside this module can read a cluster's Ceph state without
// importing the machinery that produced it — and without reimplementing the
// judgement about what Ceph's own health strings mean.
package ceph

import (
	"time"

	"github.com/runlevel-six/sextant/pkg/subsystem"
)

// KeyState holds a [State].
const KeyState = "ceph/state"

// Check is one health check Ceph is reporting.
type Check struct {
	Name     string
	Severity string
	Message  string
}

// Mons is the monitor quorum.
type Mons struct {
	Total    int
	InQuorum int
}

// Healthy reports whether every monitor is in quorum.
func (m Mons) Healthy() bool { return m.Total > 0 && m.InQuorum == m.Total }

// Mgr is the manager summary.
type Mgr struct {
	Available bool
	Standbys  int
	// Active is the active manager's name. Empty means the name was not
	// reported, which is not the same as there being none — see ActiveUnknown.
	Active  string
	Modules int
}

// ActiveUnknown reports that a manager is available but unnamed.
//
// Current releases emit a summary-only mgrmap with no active_name, so an empty
// name alongside Available is a gap in the data rather than a missing manager.
// Rendering it as "no active manager" would be a false alarm.
func (m Mgr) ActiveUnknown() bool { return m.Available && m.Active == "" }

// OSDs is the object-store daemon summary.
type OSDs struct {
	Total      int
	Up         int
	In         int
	RemappedPG int
	Epoch      int64
}

// Healthy reports whether every OSD is both up and in.
func (o OSDs) Healthy() bool { return o.Total > 0 && o.Up == o.Total && o.In == o.Total }

// PGState is one placement-group state and its count.
type PGState struct {
	Name  string
	Count int64
}

// PGs is the placement-group and capacity summary.
type PGs struct {
	Total   int64
	Pools   int64
	Objects int64
	ByState []PGState
	// DataBytes is stored data before replication; UsedBytes includes it. The
	// distinction matters: a three-way replicated pool uses roughly three times
	// its data, and reporting DataBytes as usage would understate a nearly full
	// cluster by that factor.
	DataBytes  int64
	UsedBytes  int64
	AvailBytes int64
	TotalBytes int64
}

// CleanPGs returns the count of placement groups in a fully clean state.
func (p PGs) CleanPGs() int64 {
	var n int64
	for _, s := range p.ByState {
		if s.Name == "active+clean" {
			n += s.Count
		}
	}
	return n
}

// AllClean reports whether every placement group is active+clean.
func (p PGs) AllClean() bool { return p.Total > 0 && p.CleanPGs() == p.Total }

// UsedPercent returns raw capacity used, or -1 when unknown.
func (p PGs) UsedPercent() int {
	if p.TotalBytes <= 0 {
		return -1
	}
	return int(p.UsedBytes * 100 / p.TotalBytes)
}

// IO is current client throughput.
type IO struct {
	ReadBytesPerSec  int64
	WriteBytesPerSec int64
	ReadOpsPerSec    int64
	WriteOpsPerSec   int64
}

// Status is what `ceph -s` reports.
type Status struct {
	FSID        string
	Health      string
	Checks      []Check
	MutedChecks int
	Mons        Mons
	Mgr         Mgr
	OSDs        OSDs
	PGs         PGs
	IO          IO
	// Unreadable names the sections that could not be decoded.
	Unreadable []string
}

// HealthOK reports whether Ceph itself says everything is fine.
func (s Status) HealthOK() bool { return s.Health == "HEALTH_OK" }

// State is everything the plugin publishes.
type State struct {
	Tier       subsystem.Tier
	TierReason string
	Status     Status
	// Pod names the tools pod the status came from.
	Pod       string
	UpdatedAt time.Time
	Err       error
}
