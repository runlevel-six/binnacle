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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
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
		case m.stillBroken(mg):
			out.Unresolved = append(out.Unresolved, mg)
		}
	}
	return out
}

// stillBroken reports whether a migration's instance is one Nova currently
// lists in ERROR.
func (m Migrations) stillBroken(mg Migration) bool {
	if !m.BrokenKnown || mg.InstanceUUID == "" {
		return false
	}
	_, broken := m.Broken[mg.InstanceUUID]
	return broken
}

// blocking reports whether a still-broken instance is sitting on a host someone
// is draining, which is the moment a retained failure earns a row back.
//
// The instance's *current* host is what matters, not the migration's source: a
// failed live migration often leaves the server on the destination it
// half-reached, and that is the host it will now obstruct.
func (m Migrations) blocking(mg Migration) bool {
	if !m.stillBroken(mg) {
		return false
	}
	return m.Draining[m.Broken[mg.InstanceUUID].Host]
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

// pollMigrations fetches the migration history, and the broken instances that
// decide how long a failure in it stays interesting.
//
// The whole page is requested rather than a status-filtered slice. Nova has no
// server-side filter narrow enough for what the pane wants — in flight, or
// failed within two hours — so the filtering happens here.
//
// draining is the set of disabled compute hosts as of the same publish; it is
// passed in rather than read from the store so the snapshot is self-contained
// and the pane does not have to join two independently-timed polls.
func (p *Plugin) pollMigrations(ctx context.Context, draining map[string]bool) Migrations {
	snap := Migrations{UpdatedAt: time.Now(), Draining: draining}

	client, err := p.migrationsClient(ctx)
	if err != nil {
		snap.Err = err
		return snap
	}
	snap.Broken, snap.BrokenKnown = brokenServers(ctx, client)

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

	// Last, because it reads the two things above it: which servers are broken,
	// and which migrations are in flight.
	snap.Drains = pollDrains(ctx, client, draining, snap.Items, snap.Broken)
	return snap
}

// brokenServers lists every instance Nova currently reports in ERROR, keyed by
// UUID. The second return distinguishes an empty cloud from an unanswered
// question; see [Migrations.BrokenKnown].
//
// One call, not one per row. The status filter is what makes that affordable:
// the join is against migration records, so instances in ERROR for reasons
// having nothing to do with a migration cost a map entry and no more. The
// detail listing is what Nova serves here regardless, and it is what carries
// the host and the fault — the two things that turn "this failed" into "this is
// in the way, and here is why".
//
// A failure is not the snapshot's failure. An operator whose credential can
// read migrations but not every project's servers still gets the pane; it falls
// back to the age window alone.
func brokenServers(ctx context.Context, client *gophercloud.ServiceClient) (map[string]BrokenServer, bool) {
	page, err := servers.List(client, servers.ListOpts{
		AllTenants: true,
		Status:     "ERROR",
	}).AllPages(ctx)
	if err != nil {
		return nil, false
	}
	items, err := servers.ExtractServers(page)
	if err != nil {
		return nil, false
	}

	out := make(map[string]BrokenServer, len(items))
	for _, s := range items {
		out[s.ID] = BrokenServer{
			UUID:  s.ID,
			Name:  s.Name,
			Host:  s.Host,
			Fault: strings.TrimSpace(s.Fault.Message),
		}
	}
	return out, true
}

// maxDrains bounds how many hosts the drain probe will count servers for.
//
// One call per draining host is affordable because draining is normally one
// host at a time, and a cloud with nothing disabled pays nothing at all. What
// this guards is the other shape: a cloud carrying a dozen hosts disabled for
// decommissioning would otherwise spend a dozen calls every thirty seconds
// counting servers on hardware nobody is waiting for.
const maxDrains = 8

// pollDrains measures how far each drain has got.
//
// Only Remaining costs a call. Stuck comes from the ERROR probe that already
// ran, and Moving from the migration records already fetched, so a host's whole
// line is one request — and the non-detail listing is used for it, because the
// question is how many rather than which.
func pollDrains(
	ctx context.Context,
	client *gophercloud.ServiceClient,
	draining map[string]bool,
	items []Migration,
	broken map[string]BrokenServer,
) []Drain {
	if len(draining) == 0 {
		return nil
	}

	hosts := make([]string, 0, len(draining))
	for h := range draining {
		hosts = append(hosts, h)
	}
	// Sorted before truncating, so which hosts get probed does not change from
	// poll to poll with Go's map ordering and make the block flicker.
	sort.Strings(hosts)
	if len(hosts) > maxDrains {
		hosts = hosts[:maxDrains]
	}

	// Deduped, so a server retrying its migration counts once toward Moving.
	latest := LatestPerServer(items)
	moving := map[string]int{}
	for _, m := range latest {
		if Active(m.Status) {
			moving[m.SourceCompute]++
		}
	}
	stuck := map[string]int{}
	for _, b := range broken {
		stuck[b.Host]++
	}

	out := make([]Drain, 0, len(hosts))
	for _, h := range hosts {
		d := Drain{Host: h, Moving: moving[h], Stuck: stuck[h]}
		n, err := countServersOn(ctx, client, h)
		if err != nil {
			d.Err = err
		} else {
			d.Remaining = n
		}
		out = append(out, d)
	}
	return out
}

// countServersOn counts the servers Nova still places on one compute host.
func countServersOn(ctx context.Context, client *gophercloud.ServiceClient, host string) (int, error) {
	page, err := servers.ListSimple(client, servers.ListOpts{
		AllTenants: true,
		Host:       host,
	}).AllPages(ctx)
	if err != nil {
		return 0, err
	}
	items, err := servers.ExtractServers(page)
	if err != nil {
		return 0, err
	}
	return len(items), nil
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
