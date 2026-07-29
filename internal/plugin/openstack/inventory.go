package openstack

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// KeyInventory holds an Inventory.
const KeyInventory = "openstack/inventory"

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

// counter is one line of the inventory: a label, whether a state breakdown is
// worth showing, and how to fetch it.
type counter struct {
	label     string
	withState bool
	fetch     func(context.Context, *gophercloud.ProviderClient, gophercloud.EndpointOpts) (int, map[string]int, error)
}

// counters is the inventory in display order.
//
// Identity first because the project count frames everything under it, then
// compute, then the network objects in the order an operator builds them, then
// storage. State breakdowns are on the four kinds where a stuck state is a real
// event worth seeing — a server in ERROR, a volume in error, an unattached
// floating IP, a load balancer stuck in PENDING_UPDATE. The others would just be
// a parenthesis reading "ACTIVE n" next to a total of n.
var counters = []counter{
	{label: "Projects", fetch: countProjects},
	{label: "Servers", withState: true, fetch: countServers},
	{label: "Networks", fetch: countNetworks},
	{label: "Subnets", fetch: countSubnets},
	{label: "Routers", fetch: countRouters},
	{label: "Floating IPs", withState: true, fetch: countFloatingIPs},
	{label: "Load Balancers", withState: true, fetch: countLoadBalancers},
	{label: "Volumes", withState: true, fetch: countVolumes},
}

// pollInventory counts every kind.
//
// Failures are per-kind by design; see [Count]. The one shared failure is
// authentication, which is reported on the Inventory itself since no count could
// have succeeded.
func (p *Plugin) pollInventory(ctx context.Context) Inventory {
	inv := Inventory{UpdatedAt: time.Now()}

	provider, eo, err := p.connect(ctx)
	if err != nil {
		inv.Err = err
		return inv
	}

	inv.Counts = make([]Count, 0, len(counters))
	for _, c := range counters {
		total, byState, err := c.fetch(ctx, provider, eo)
		count := Count{Label: c.label}
		switch {
		case notDeployed(err):
			count.Absent = true
		case err != nil:
			count.Err = err
		default:
			count.Total = total
			if c.withState {
				count.ByState = byState
			}
		}
		inv.Counts = append(inv.Counts, count)
	}
	return inv
}

// notDeployed reports whether an error means the service is absent from the
// catalog rather than broken.
//
// A cloud without Octavia is not a cloud with a failing load balancer service,
// and rendering it red would teach the operator to ignore that row — which is
// the row that matters on the day Octavia really does break.
func notDeployed(err error) bool {
	var e gophercloud.ErrEndpointNotFound
	return errors.As(err, &e)
}

func countProjects(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewIdentityV3(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := projects.List(client, projects.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := projects.ExtractProjects(page)
	if err != nil {
		return 0, nil, err
	}
	return len(items), nil, nil
}

// countServers tallies every server in the cloud by status.
//
// AllTenants is required: without it the count covers only the credential's own
// project, which on an admin credential is close to empty and reads as an idle
// cloud.
func countServers(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := servers.List(client, servers.ListOpts{AllTenants: true}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := servers.ExtractServers(page)
	if err != nil {
		return 0, nil, err
	}
	by := map[string]int{}
	for _, s := range items {
		by[strings.ToUpper(s.Status)]++
	}
	return len(items), by, nil
}

func countNetworks(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := networks.List(client, networks.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := networks.ExtractNetworks(page)
	if err != nil {
		return 0, nil, err
	}
	return len(items), nil, nil
}

func countSubnets(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := subnets.List(client, subnets.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := subnets.ExtractSubnets(page)
	if err != nil {
		return 0, nil, err
	}
	return len(items), nil, nil
}

func countRouters(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := routers.List(client, routers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := routers.ExtractRouters(page)
	if err != nil {
		return 0, nil, err
	}
	return len(items), nil, nil
}

// countFloatingIPs reports in-use against free rather than Neutron's status
// field.
//
// A floating IP's useful state is whether it is attached to a port, which is
// what the quota conversation is about; its `status` mirrors that with less
// nuance. Deriving it here saves the pane a second call.
func countFloatingIPs(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := floatingips.List(client, floatingips.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := floatingips.ExtractFloatingIPs(page)
	if err != nil {
		return 0, nil, err
	}
	by := map[string]int{}
	for _, f := range items {
		if f.PortID != "" {
			by["IN-USE"]++
		} else {
			by["FREE"]++
		}
	}
	return len(items), by, nil
}

// countLoadBalancers breaks down by provisioning status.
//
// Provisioning status answers "is this load balancer settled", which is the
// question during an upgrade — a PENDING_UPDATE that never clears is a stuck
// Octavia. Operating status (ONLINE, DEGRADED) is the live data-plane signal and
// far more transient, so it is left out to keep the row scannable.
func countLoadBalancers(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewLoadBalancerV2(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := loadbalancers.List(client, loadbalancers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := loadbalancers.ExtractLoadBalancers(page)
	if err != nil {
		return 0, nil, err
	}
	by := map[string]int{}
	for _, lb := range items {
		by[strings.ToUpper(lb.ProvisioningStatus)]++
	}
	return len(items), by, nil
}

func countVolumes(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (int, map[string]int, error) {
	client, err := openstack.NewBlockStorageV3(provider, eo)
	if err != nil {
		return 0, nil, err
	}
	page, err := volumes.List(client, volumes.ListOpts{AllTenants: true}).AllPages(ctx)
	if err != nil {
		return 0, nil, err
	}
	items, err := volumes.ExtractVolumes(page)
	if err != nil {
		return 0, nil, err
	}
	by := map[string]int{}
	for _, v := range items {
		by[strings.ToUpper(v.Status)]++
	}
	return len(items), by, nil
}
