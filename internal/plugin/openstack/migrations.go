package openstack

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/utils"
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
// which is deliberately not depended on. migration_type is present only when the
// request negotiated microversion 2.23 or better — see
// [migrationMicroversions] — and a cloud that cannot offer that sends rows with
// an empty kind rather than failing to parse.
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

// migrationMicroversions are the Nova microversions this poll can use, best
// first. The first one the cloud supports is the one it asks for.
//
// A microversion has to be asked for. Nothing is negotiated by default: with no
// X-OpenStack-Nova-API-Version header gophercloud gets Nova's *minimum*, 2.1,
// whatever the cloud is capable of. That default cost this pane two things.
//
// 2.23 is where /os-migrations starts returning migration_type. Below it Nova
// strips the field from the response entirely, so the pane's type column was
// blank on every cloud — not, as the code here used to claim, only on clouds too
// old to send it.
//
// 2.59 is where the endpoint becomes sorted (created_at desc, id desc) and
// paginated. Below it there is neither: Nova returns the entire migrations table
// in one response, every poll, forever. A two-year-old cloud answered the 2.1
// request with 7113 records and the 2.59 request with 1000, to render at most a
// dozen rows.
var migrationMicroversions = []string{"2.59", "2.23"}

// migrationLimit bounds one page of /os-migrations.
//
// Truncation is safe for [LatestPerServer] because of how 2.59 sorts: newest
// first, globally. A server's most recent record therefore always appears before
// its older ones, so a cut can only discard history the pane had already decided
// not to show — never the record that decides which row a server gets.
//
// The number is Nova's own default api.max_limit. Asking for it explicitly means
// the request is bounded by this program rather than by whatever the cloud's
// operator set, which is the point of asking at all.
const migrationLimit = 1000

// pollMigrations fetches the migration history.
//
// The whole page is requested rather than a status-filtered slice. Nova has no
// server-side filter narrow enough for what the pane wants — in flight, or
// failed within two hours — so the filtering happens here.
func (p *Plugin) pollMigrations(ctx context.Context) Migrations {
	snap := Migrations{UpdatedAt: time.Now()}

	client, err := p.migrationsClient(ctx)
	if err != nil {
		snap.Err = err
		return snap
	}

	url := client.ServiceURL("os-migrations")
	if supportsMigrationPaging(client.Microversion) {
		url += "?limit=" + strconv.Itoa(migrationLimit)
	}

	var resp migrationsResponse
	if _, err := client.Get(ctx, url, &resp, nil); err != nil {
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
	// created_at, not updated_at: Nova stamps updated_at whenever it happens to
	// write the row, which is not the order things happened in. See
	// [LatestPerServer].
	sort.Slice(snap.Items, func(i, j int) bool {
		if !snap.Items[i].CreatedAt.Equal(snap.Items[j].CreatedAt) {
			return snap.Items[i].CreatedAt.After(snap.Items[j].CreatedAt)
		}
		return snap.Items[i].ID > snap.Items[j].ID
	})
	return snap
}

// migrationsClient builds the compute client for the migration poll, with the
// best microversion this cloud offers.
func (p *Plugin) migrationsClient(ctx context.Context) (*gophercloud.ServiceClient, error) {
	provider, eo, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	client, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, err
	}
	client.Microversion = p.migrationsMicroversion(ctx, client)
	return client, nil
}

// migrationsMicroversion picks the first supported entry of
// [migrationMicroversions], and caches the verdict.
//
// Negotiating costs a GET of the compute endpoint's version document, which is
// worth paying once and not once every thirty seconds. A cloud that answers and
// offers nothing usable is cached as the empty string — a settled answer, since
// a running cloud does not gain microversions.
//
// A negotiation that *fails* is not cached, and that asymmetry is the point.
// This dashboard is watched during control-plane rollouts, so the likeliest
// moment for the version document to be unreachable is the first poll of the
// session. Caching that would silently pin the whole run to 2.1 — blank type
// column, unbounded fetch — over one bad request. Leaving it unresolved costs
// one cheap GET per poll until the cloud answers, and then it settles.
func (p *Plugin) migrationsMicroversion(ctx context.Context, client *gophercloud.ServiceClient) string {
	if p.migrationsMVKnown {
		return p.migrationsMV
	}

	supported, err := utils.GetSupportedMicroversions(ctx, client)
	if err != nil {
		return ""
	}

	p.migrationsMVKnown = true
	for _, want := range migrationMicroversions {
		if ok, err := supported.IsSupported(want); err == nil && ok {
			p.migrationsMV = want
			break
		}
	}
	return p.migrationsMV
}

// supportsMigrationPaging reports whether a negotiated microversion takes the
// limit parameter on /os-migrations.
//
// Before 2.59 the endpoint has no pagination and validates no query parameters,
// so the parameter would be silently ignored rather than refused — which is
// worse than not sending it, because the code would look like it had asked.
func supportsMigrationPaging(microversion string) bool {
	major, minor, err := utils.ParseMicroversion(microversion)
	if err != nil {
		return false
	}
	return major > 2 || (major == 2 && minor >= 59)
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
