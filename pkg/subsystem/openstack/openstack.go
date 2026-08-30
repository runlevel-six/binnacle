// Package openstack holds the state sextant's OpenStack plugin publishes.
//
// The types are here, separate from the plugin that fills them, so a consumer
// outside this module can read a cloud's state without importing gophercloud or
// the polling that produced it.
//
// The interpretation comes with them, deliberately. Which migrations are still
// worth showing, what a status string means, which host is draining — those are
// decisions, not fields, and a second front end deriving them again would
// eventually disagree with the first.
package openstack

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Store keys. Each holds the type its comment names.
const (
	// KeyState holds a [State].
	KeyState = "openstack/state"
	// KeyMigrations holds a [Migrations].
	KeyMigrations = "openstack/migrations"
	// KeyInventory holds an [Inventory].
	KeyInventory = "openstack/inventory"
)

// Count is one resource kind's tally.
//
// Each kind carries its own error, because each is a separate API call against a
// separate service. A denied Keystone, an undeployed Octavia and a slow Nova are
// three independent facts, and collapsing them into one pane-wide failure would
// hide the seven counts that did come back.
type Count struct {
	// Label is the human-readable kind, e.g. "Floating IPs".
	Label string
	Total int
	// ByState breaks the total down by the kind's own status vocabulary —
	// server power states, volume states, load balancer provisioning states.
	// Empty for kinds where a breakdown is noise.
	ByState map[string]int
	// Absent reports that the service is not in the catalog. Distinct from Err:
	// a cloud with no Octavia is correctly configured, not broken.
	Absent bool
	Err    error
}

// Inventory is the cloud-wide resource count the at-rest pane shows.
type Inventory struct {
	// Counts is one entry per kind, in a fixed order chosen for reading rather
	// than sorted: identity, then compute, then network, then storage.
	Counts    []Count
	UpdatedAt time.Time
	// Err is set only when nothing could be counted at all — an authentication
	// failure, rather than one service being unavailable.
	Err error
}

// FailedWindow is how long a failed migration stays on screen after its last
// update.
//
// Two hours covers an upgrade window the operator is paying attention to,
// without carrying yesterday's retries into today's screen. It also spans both
// flavors of retry: an operator retry creates a new record with a new
// updated_at, while a scheduler retry updates the existing one — either way the
// row is still there when they look.
const FailedWindow = 2 * time.Hour

// Migration is one Nova server migration.
//
// Timestamps are normalised to time.Time here rather than in the pane, so the
// recency window is a comparison and not a parse.
type Migration struct {
	ID     int64
	Status string
	// Type is the migration's kind: "live-migration", "migration" (a cold
	// resize), "evacuation", "resize".
	Type string
	// InstanceUUID identifies the server being moved. It is the dedup key.
	InstanceUUID  string
	SourceCompute string
	DestCompute   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// BrokenServer is what Nova says about an instance currently in ERROR.
//
// Host is where the instance actually is, which after a failed live migration
// is often the destination it half-landed on rather than the source it was
// meant to leave. That is the field that decides whether a stale failure is in
// the way of the drain happening now.
type BrokenServer struct {
	UUID string
	Name string
	Host string
	// Fault is Nova's own explanation, when it gave one.
	Fault string
}

// Drain is one compute host being emptied, and how far along it is.
//
// This is the question the migration table only answers sideways. A list of
// moves in flight says what is happening; it does not say whether the host is
// nearly empty, whether anything is moving at all, or whether what is left
// cannot be moved. Those are the three things an operator waiting on a drain is
// actually watching for, and the last two look identical on a migration table:
// an empty table means "finished" and "stalled" alike.
type Drain struct {
	Host string
	// Remaining is how many servers Nova still places on this host. Zero is the
	// signal the operator is waiting for — the drain is done and the
	// maintenance can start.
	Remaining int
	// Moving is how many of those have a migration in flight right now. Zero
	// with a non-zero Remaining is a stalled drain.
	Moving int
	// Stuck is how many are in ERROR, and so cannot be live-migrated at all.
	// These are what a drain ends up blocked on.
	Stuck int
	// Err records a per-host failure, so one unreadable host costs its own line
	// rather than the whole block.
	Err error
}

// Migrations is what the migration poll publishes: the cloud's full recent
// migration history, newest first, plus the context needed to judge which of it
// still matters.
//
// The history is published unfiltered. Deciding which rows are worth showing is
// a display question that depends on the clock, and a snapshot that has already
// discarded everything but the interesting rows cannot answer a different
// question later.
type Migrations struct {
	Items []Migration
	// Broken maps instance UUID to what Nova reports about servers in ERROR,
	// read in the same poll as Items so the join cannot tear across two.
	Broken map[string]BrokenServer
	// BrokenKnown separates "nothing is broken" from "could not ask". A
	// credential without all-tenants access, or a Nova mid-rollout, leaves this
	// false — see [Migrations.Relevant], where it can only ever extend a row's
	// life and never cut one short.
	BrokenKnown bool
	// Draining is the set of compute hosts an operator has disabled, captured
	// at the same instant. A disabled host is how a drain looks; see the
	// package comment.
	Draining map[string]bool
	// Drains is the progress of each host in Draining, host-sorted, and capped
	// at [maxDrains]. Shorter than Draining means the rest went unprobed; the
	// pane says so rather than implying the cloud has fewer drains than it has.
	Drains    []Drain
	UpdatedAt time.Time
	Err       error
}

// Active reports whether a migration is still in flight.
//
// Anything not listed here and not a failure is treated as finished, which is
// the safe default for a status vocabulary that grows: an unknown state
// disappears from the pane rather than sticking there forever.
func Active(status string) bool {
	switch strings.ToLower(status) {
	case "queued", "preparing", "accepted", "pre-migrating", "migrating", "running", "post-migrating":
		return true
	}
	return false
}

// Failed reports whether a migration ended badly.
func Failed(status string) bool {
	s := strings.ToLower(status)
	return s == "failed" || s == "error"
}

// LatestPerServer collapses the history to one row per server, keeping the most
// recent attempt.
//
// A single server's retry sequence — queued, running, failed, queued again —
// is one story, and showing four rows for it pushes the other servers off the
// pane during exactly the upgrade where every server is moving. Failures sort
// first, then newest, so the rows needing attention are at the top whatever the
// pane's height.
//
// # Which record is the latest
//
// The ID, not updated_at. This is not a stylistic preference; ordering on
// updated_at is wrong and produced the bug this function exists to avoid.
//
// updated_at is when Nova last *wrote the row*, not when the migration ended,
// and Nova writes rows well out of order. On a live cloud mid-drain, record
// 70551 was created at 17:22:22 and stamped updated_at 17:37:47 — after the
// eight subsequent migrations of that same server had already finished. Ordering
// its history by updated_at picks a fifteen-minute-old record as current and
// renders that record's source and destination, so the pane names a pair of
// hosts the server is not moving between. Measured on that cloud: 112 ordering
// inversions in 500 consecutive IDs, and six servers whose history resolved to
// the wrong record. Ordering the same data by ID reproduced every server's true
// location as reported by `openstack server show`.
//
// The ID is a per-cell autoincrement, so it is monotonic exactly where this
// needs it to be: within one server's history, since an instance does not change
// cell. Across servers it means nothing, which is why the display sort below
// leads with created_at and uses the ID only to break ties.
//
// Records with no instance UUID are kept individually, keyed by ID. That should
// not happen, but dropping data over a schema surprise is worse than an extra
// row.
func LatestPerServer(items []Migration) []Migration {
	latest := make(map[string]Migration, len(items))
	for _, m := range items {
		key := m.InstanceUUID
		if key == "" {
			key = fmt.Sprintf("id:%d", m.ID)
		}
		if cur, seen := latest[key]; !seen || m.ID > cur.ID {
			latest[key] = m
		}
	}

	out := make([]Migration, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		fi, fj := Failed(out[i].Status), Failed(out[j].Status)
		if fi != fj {
			return fi
		}
		// created_at rather than updated_at, for the reason above, and rather
		// than the ID, which is not comparable between two servers that may sit
		// in different cells. Nova writes created_at once, when conductor
		// records the attempt, so it is the honest "when did this start".
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// ShortType abbreviates Nova's migration_type for display.
//
// The type column is very nearly a constant: during a hypervisor drain every row
// says live-migration, so fourteen columns are spent distinguishing a case that
// rarely varies. Those are exactly the columns the source-to-destination pair is
// short of — a pair of real compute hostnames needs thirty-one, and one grid
// column in four leaves twenty-four. The distinction still matters when it does
// vary, since an evacuation means the source is already gone, so the column stays
// rather than being dropped.
func ShortType(t string) string {
	switch t {
	case "live-migration":
		return "live"
	case "evacuation":
		return "evac"
	case "migration":
		return "cold"
	}
	return t
}

// ShortStatus abbreviates the two Nova statuses long enough to cost another
// column its content, and leaves every other one alone.
//
// "post-migrating" and "pre-migrating" are fourteen and thirteen columns for a
// prefix that carries all of the meaning; abbreviating them keeps the widest
// status at ten, which is what lets a pair of real compute hostnames fit beside
// it. Nothing else is touched — a status is data, and rewriting one that already
// fits would be presentation dressed up as economy.
func ShortStatus(s string) string {
	switch s {
	case "post-migrating":
		return "post-mig"
	case "pre-migrating":
		return "pre-mig"
	}
	return s
}

// Shown is the split a pane needs: the rows worth listing at this size, and the
// unresolved failures worth counting but not spending a row on.
type Shown struct {
	// Rows is what the compact pane lists, failures first.
	Rows []Migration
	// Unresolved is every failure whose instance is still in ERROR and whose
	// host nobody is draining right now. Named in the summary line and listed
	// only when the pane has rows to spare, which is what zoom is for.
	Unresolved []Migration
}

// Failures counts the rows in Rows that ended badly.
func (s Shown) Failures() int {
	n := 0
	for _, m := range s.Rows {
		if Failed(m.Status) {
			n++
		}
	}
	return n
}

// Relevant decides which of the history is worth the operator's attention now.
//
// Everything in flight is listed, as is any failure inside [FailedWindow] —
// "this just happened" is worth a row whether or not the instance recovered,
// because a migration that failed is a server that did not leave the host.
//
// # Unresolved failures are retained, not expired
//
// A failure whose instance is still in ERROR is never dropped on age. It cannot
// be live-migrated, so it is a landmine: it costs nothing until the rollout
// reaches its host, and then it costs the drain. Expiring it on a timer
// disarms the detector before the next upgrade — a two-day rollout means last
// week's breakage is invisible exactly when it is about to matter.
//
// What is bounded is not how long such a row is *kept* but when it takes up
// space. It is listed while its instance sits on a host being drained now, and
// counted the rest of the time. So a stale-VM backlog cannot crowd out the live
// drain, and nothing disappears silently either.
//
// When the ERROR probe could not run, BrokenKnown is false and no row is ever
// promoted or retained on this path — the behavior falls back to the window
// alone. The probe can only extend a row's life. A probe that failed must not
// be able to hide a failure, which would be a worse bug than the one this
// solves.
func (m Migrations) Relevant(now time.Time) Shown {
	items := LatestPerServer(m.Items)
	out := Shown{Rows: make([]Migration, 0, len(items))}
	for _, mg := range items {
		switch {
		case Active(mg.Status):
			out.Rows = append(out.Rows, mg)
		case !Failed(mg.Status):
			// A terminal success, or a status this build does not know. Both
			// leave the pane; see [Active].
		case now.Sub(mg.UpdatedAt) <= FailedWindow:
			out.Rows = append(out.Rows, mg)
		case m.blocking(mg):
			out.Rows = append(out.Rows, mg)
		case m.StillBroken(mg):
			out.Unresolved = append(out.Unresolved, mg)
		}
	}
	return out
}

// Agent is one service agent, from whichever OpenStack service reported it.
type Agent struct {
	// Service is which OpenStack service this came from: "compute", "network" or
	// "block-storage".
	Service string
	// Binary is the agent process, e.g. "nova-compute", "neutron-ovn-metadata-agent".
	Binary string
	// Host is the node or pod running it.
	Host string
	// Zone is the availability zone, or "internal" for a control-plane service.
	Zone string
	// Up reports whether the agent is reporting in.
	Up bool
	// Enabled reports whether the scheduler is allowed to use it. A disabled
	// agent is a deliberate act, not a fault — see the package comment.
	Enabled bool
	// UpdatedAt is when the agent last checked in.
	UpdatedAt time.Time
}

// Healthy reports whether an agent needs no attention: up, or deliberately
// disabled.
//
// A disabled agent is excluded rather than counted as broken, because that is what
// draining a compute node looks like and it must not read as a failure.
func (a Agent) Healthy() bool { return a.Up || !a.Enabled }

// ServiceSummary groups one OpenStack service's agents.
type ServiceSummary struct {
	Service string
	Total   int
	Up      int
	// Disabled counts agents an operator has taken out of service.
	Disabled int
	// DownBinaries names the binaries with agents that are down and still
	// enabled, so the summary says what is broken rather than only that
	// something is.
	DownBinaries []string
	Err          error
}

// Healthy reports whether every enabled agent in this service is up.
func (s ServiceSummary) Healthy() bool {
	return s.Err == nil && len(s.DownBinaries) == 0
}

// State is everything the plugin publishes.
type State struct {
	// Cloud is the clouds.yaml profile in use.
	Cloud string
	// Region is the region the endpoints resolved to.
	Region string
	// Services is one summary per OpenStack service, in a stable order.
	Services []ServiceSummary
	// Agents is every agent, for the detail table.
	Agents    []Agent
	UpdatedAt time.Time
	Err       error
}

// DrainingHosts is the set of compute hosts an operator has taken out of the
// scheduler, which is what draining a hypervisor looks like from the API.
//
// Only nova-compute is considered. A disabled neutron agent is a different
// event, and a host is not being emptied of servers because its metadata agent
// is off.
//
// The name overstates it slightly and deliberately: a host disabled for a dead
// power supply is not being drained either, and this will call it draining. The
// rest of the plugin already reads disabled as intentional-not-broken — see the
// package comment — so this is the same heuristic rather than a second one, and
// the cost of the false positive is one extra row on a pane.
func DrainingHosts(s State) map[string]bool {
	out := map[string]bool{}
	for _, a := range s.Agents {
		if a.Service == ServiceCompute && a.Binary == "nova-compute" && !a.Enabled {
			out[a.Host] = true
		}
	}
	return out
}

// DownAgents returns the agents that are down while still enabled — the ones that
// need attention.
func (s State) DownAgents() []Agent {
	var out []Agent
	for _, a := range s.Agents {
		if !a.Healthy() {
			out = append(out, a)
		}
	}
	return out
}

// DisabledAgents returns the agents an operator has taken out of service.
func (s State) DisabledAgents() []Agent {
	var out []Agent
	for _, a := range s.Agents {
		if !a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

// blocking reports whether a still-broken instance is sitting on a host someone
// is draining, which is the moment a retained failure earns a row back.
//
// The instance's *current* host is what matters, not the migration's source: a
// failed live migration often leaves the server on the destination it
// half-reached, and that is the host it will now obstruct.
func (m Migrations) blocking(mg Migration) bool {
	if !m.StillBroken(mg) {
		return false
	}
	return m.Draining[m.Broken[mg.InstanceUUID].Host]
}

// StillBroken reports whether a migration's instance is one Nova currently
// lists in ERROR.
func (m Migrations) StillBroken(mg Migration) bool {
	if !m.BrokenKnown || mg.InstanceUUID == "" {
		return false
	}
	_, broken := m.Broken[mg.InstanceUUID]
	return broken
}

// Service names, used as the grouping key.
const (
	ServiceCompute      = "compute"
	ServiceNetwork      = "network"
	ServiceBlockStorage = "block-storage"
)
