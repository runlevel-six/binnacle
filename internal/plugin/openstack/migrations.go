package openstack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack"
)

// KeyMigrations holds a Migrations.
const KeyMigrations = "openstack/migrations"

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

// Migrations is what the migration poll publishes: the cloud's full recent
// migration history, newest first.
//
// The history is published unfiltered. Deciding which rows are worth showing is
// a display question that depends on the clock, and a snapshot that has already
// discarded everything but the interesting rows cannot answer a different
// question later.
type Migrations struct {
	Items     []Migration
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
// first, then most-recently-updated, so the rows needing attention are at the
// top whatever the pane's height.
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
		cur, seen := latest[key]
		switch {
		case !seen:
			latest[key] = m
		case m.UpdatedAt.After(cur.UpdatedAt):
			latest[key] = m
		case m.UpdatedAt.Equal(cur.UpdatedAt) && m.ID > cur.ID:
			// Same timestamp is common — Nova's resolution is coarser than a
			// retry loop — so the higher ID breaks the tie deterministically.
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
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
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

// Relevant keeps the migrations worth showing at now: everything in flight, plus
// failures inside [FailedWindow].
func Relevant(items []Migration, now time.Time) []Migration {
	out := make([]Migration, 0, len(items))
	for _, m := range items {
		switch {
		case Active(m.Status):
			out = append(out, m)
		case Failed(m.Status) && now.Sub(m.UpdatedAt) <= FailedWindow:
			out = append(out, m)
		}
	}
	return out
}

// migrationsResponse is the wire shape of GET /os-migrations.
type migrationsResponse struct {
	Migrations []migrationEntry `json:"migrations"`
}

// migrationEntry mirrors the wire format.
//
// instance_uuid has been the canonical field on /os-migrations since the
// endpoint shipped; later microversions also expose server_uuid as a duplicate,
// which is deliberately not depended on. migration_type arrived at microversion
// 2.23 — a cloud older than that sends "type" instead, and its rows will show an
// empty kind rather than failing to parse.
type migrationEntry struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	MigrationType string `json:"migration_type"`
	InstanceUUID  string `json:"instance_uuid"`
	SourceCompute string `json:"source_compute"`
	DestCompute   string `json:"dest_compute"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// pollMigrations fetches the migration history.
//
// The whole list is requested rather than a status-filtered slice. Nova has no
// server-side filter narrow enough for what the pane wants — in flight, or
// failed within two hours — and the result set is a few hundred records even on
// a busy cloud, so the filtering happens here.
func (p *Plugin) pollMigrations(ctx context.Context) Migrations {
	snap := Migrations{UpdatedAt: time.Now()}

	provider, eo, err := p.connect(ctx)
	if err != nil {
		snap.Err = err
		return snap
	}
	client, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		snap.Err = err
		return snap
	}

	var resp migrationsResponse
	if _, err := client.Get(ctx, client.ServiceURL("os-migrations"), &resp, nil); err != nil {
		snap.Err = err
		return snap
	}

	snap.Items = make([]Migration, 0, len(resp.Migrations))
	for _, m := range resp.Migrations {
		snap.Items = append(snap.Items, Migration{
			ID:            m.ID,
			Status:        m.Status,
			Type:          m.MigrationType,
			InstanceUUID:  m.InstanceUUID,
			SourceCompute: m.SourceCompute,
			DestCompute:   m.DestCompute,
			CreatedAt:     parseNovaTime(m.CreatedAt),
			UpdatedAt:     parseNovaTime(m.UpdatedAt),
		})
	}
	sort.Slice(snap.Items, func(i, j int) bool {
		return snap.Items[i].UpdatedAt.After(snap.Items[j].UpdatedAt)
	})
	return snap
}

// parseNovaTime accepts the timestamp formats /os-migrations returns.
//
// The usual shape is "2006-01-02T15:04:05.000000" with no zone, since Nova emits
// UTC, but the RFC3339 variants are accepted too in case a later microversion
// changes shape. An unparseable value yields the zero time, which reads as an
// unknown age rather than as an error — one odd timestamp should not cost the
// operator the rest of the row.
func parseNovaTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000000",
		"2006-01-02T15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
