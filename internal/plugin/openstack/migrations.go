package openstack

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	osstate "github.com/runlevel-six/sextant/pkg/subsystem/openstack"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/utils"
)

// KeyMigrations holds a Migrations.
const KeyMigrations = osstate.KeyMigrations

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
