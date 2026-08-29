package openstack

import (
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

func wl(ns, app, part, kind string, desired, updated int32, manual bool) model.Workload {
	name := app
	if part != "" {
		name = app + "-" + part
	}
	return model.Workload{
		Namespace: ns, Name: name, Kind: kind,
		Desired: desired, Updated: updated, Ready: desired, Manual: manual,
		Labels: map[string]string{"application": app, "component": part},
	}
}

func withWorkloads(items ...model.Workload) *store.Store {
	s := store.New()
	s.Put(model.KeyWorkloadWorkloads, model.Snapshot[model.Workload]{Items: items})
	return s
}

// The point of the whole view: services with no agent registry are invisible to
// the cloud API, so they have to come from the cluster or not at all.
func TestCollectServicesSeesAgentlessServices(t *testing.T) {
	s := withWorkloads(
		wl("openstack", "keystone", "api", "Deployment", 3, 3, false),
		wl("openstack", "glance", "api", "Deployment", 3, 3, false),
		wl("openstack", "nova", "api", "Deployment", 3, 3, false),
		wl("openstack", "neutron", "server", "Deployment", 3, 3, false),
		wl("openstack", "placement", "api", "Deployment", 3, 3, false),
	)

	svcs, ok := CollectServices(s, "")
	if !ok {
		t.Fatal("CollectServices found nothing")
	}
	if svcs.Namespace != "openstack" {
		t.Errorf("namespace = %q, want openstack", svcs.Namespace)
	}
	names := map[string]bool{}
	for _, svc := range svcs.Items {
		names[svc.Name] = true
	}
	// Keystone, Glance and Placement register no agents anywhere in OpenStack.
	for _, want := range []string{"keystone", "glance", "placement"} {
		if !names[want] {
			t.Errorf("agentless service %q is missing; the agent API cannot see it either", want)
		}
	}
	if !svcs.Converged() {
		t.Error("every workload is up to date but the set reports as rolling")
	}
}

// A service is several workloads, and the sum is the answer. Splitting one
// service across a row per Deployment turns one question into ten.
func TestCollectServicesSumsComponentsAndNamesTheLaggard(t *testing.T) {
	s := withWorkloads(
		wl("openstack", "nova", "os-api", "Deployment", 3, 3, false),
		wl("openstack", "nova", "conductor", "Deployment", 3, 3, false),
		wl("openstack", "nova", "compute", "DaemonSet", 5, 2, false),
	)

	svcs, _ := CollectServices(s, "openstack")
	if len(svcs.Items) != 1 {
		t.Fatalf("got %d services, want 1: %+v", len(svcs.Items), svcs.Items)
	}
	nova := svcs.Items[0]
	if nova.Desired != 11 || nova.Updated != 8 {
		t.Errorf("nova = %d/%d up to date, want 8/11", nova.Updated, nova.Desired)
	}
	if nova.Stale() != 3 {
		t.Errorf("nova stale = %d, want 3", nova.Stale())
	}

	// The service name says something is stuck; only the component says what.
	behind := nova.Behind()
	if len(behind) != 1 || TrimComponent("nova", behind[0].Name) != "compute" {
		t.Fatalf("behind = %+v, want just nova-compute", behind)
	}
}

// Manual is true only when every workload behind a service is OnDelete. A mixed
// service has a half the controller is already replacing, and calling the whole
// thing manual sends someone hunting for pods that are being deleted for them.
func TestServiceIsManualOnlyWhenEveryWorkloadIs(t *testing.T) {
	mixed, _ := CollectServices(withWorkloads(
		wl("openstack", "nova", "api", "Deployment", 3, 1, false),
		wl("openstack", "nova", "compute", "DaemonSet", 5, 1, true),
	), "openstack")
	if mixed.Items[0].Manual {
		t.Error("a service with one rolling workload is reported as manual")
	}
	if mixed.NeedsOperator() {
		t.Error("a mixed service asks for an operator; the controller is already replacing half of it")
	}

	all, _ := CollectServices(withWorkloads(
		wl("openstack", "libvirt", "libvirt", "DaemonSet", 5, 1, true),
	), "openstack")
	if !all.Items[0].Manual {
		t.Error("an all-OnDelete service is not reported as manual")
	}
	if !all.NeedsOperator() {
		t.Error("an all-OnDelete service behind its template does not ask for an operator")
	}
}

// The empty DaemonSets a per-node-configuration split leaves behind are not idle
// services. Counting them adds rows that can never say anything.
func TestCollectServicesSkipsEmptyWorkloads(t *testing.T) {
	svcs, _ := CollectServices(withWorkloads(
		wl("openstack", "keystone", "api", "Deployment", 3, 3, false),
		wl("openstack", "openvswitch", "server", "DaemonSet", 0, 0, true),
	), "openstack")

	for _, svc := range svcs.Items {
		if svc.Name == "openvswitch" {
			t.Errorf("an empty workload became a service: %+v", svc)
		}
	}
}

// The namespace is derived, because every hardcoded namespace in this codebase
// has been wrong on the first real cluster it met. A stray labeled object
// elsewhere must not outrank the actual control plane.
func TestCollectServicesDerivesNamespaceByServiceCount(t *testing.T) {
	svcs, ok := CollectServices(withWorkloads(
		wl("other-ns", "nova", "api", "Deployment", 9, 9, false),
		wl("cloud", "keystone", "api", "Deployment", 1, 1, false),
		wl("cloud", "nova", "api", "Deployment", 1, 1, false),
		wl("cloud", "glance", "api", "Deployment", 1, 1, false),
	), "")
	if !ok || svcs.Namespace != "cloud" {
		t.Errorf("namespace = %q, want cloud (3 services beats 1 large one)", svcs.Namespace)
	}
}

// Before the snapshot arrives the honest answer is that we do not know. Reporting
// a finished rollout we have not observed is the one wrong answer that reads as
// good news.
func TestServicesNotConvergedBeforeAnythingIsKnown(t *testing.T) {
	if (Services{}).Converged() {
		t.Error("an empty set reports as converged")
	}
	if _, ok := CollectServices(store.New(), ""); ok {
		t.Error("CollectServices succeeded with no snapshot in the store")
	}
}

// A converged cloud in a short frame must say how many services there are, not
// list whichever three sort first — that is the omission the pane exists to fix,
// and the renderer must not reintroduce it.
func TestServicesPaneShortAndConvergedReportsTheCount(t *testing.T) {
	items := make([]model.Workload, 0, 11)
	for _, app := range []string{
		"keystone", "glance", "nova", "neutron", "cinder", "heat",
		"octavia", "barbican", "placement", "magnum", "manila",
	} {
		items = append(items, wl("openstack", app, "api", "Deployment", 3, 3, false))
	}

	body := stripANSI(newServicesPane(withWorkloads(items...), "openstack").Render(60, 4, false))
	if !strings.Contains(body, "11 service(s) up to date") {
		t.Errorf("want the whole-cloud count in a short frame, got:\n%s", body)
	}
	if strings.Contains(body, "+ ") {
		t.Errorf("a converged cloud should not render a truncated table:\n%s", body)
	}
}

// When services are behind, the rows naming them beat the summary counting them.
func TestServicesPaneKeepsPendingRowsOverTheSummary(t *testing.T) {
	s := withWorkloads(
		wl("openstack", "keystone", "api", "Deployment", 3, 3, false),
		wl("openstack", "glance", "api", "Deployment", 3, 3, false),
		wl("openstack", "nova", "compute", "DaemonSet", 5, 2, false),
		wl("openstack", "neutron", "ovn-metadata-agent", "DaemonSet", 5, 3, false),
		wl("openstack", "libvirt", "libvirt", "DaemonSet", 5, 1, true),
	)

	body := stripANSI(newServicesPane(s, "openstack").Render(60, 4, false))
	for _, want := range []string{"libvirt", "nova", "neutron"} {
		if !strings.Contains(body, want) {
			t.Errorf("pending service %q was displaced:\n%s", want, body)
		}
	}
	// The manual marker rides on the row, which is why the summary can be the
	// thing that yields.
	if !strings.Contains(body, "⚠") {
		t.Errorf("the manual marker is missing:\n%s", body)
	}
}

// OVN and Open vSwitch live in the OpenStack namespace and are reported by the
// network frame, which lists them component by component with the nodes still to
// drain. Listing them here as well said the same thing twice — and, because both
// frames contribute a banner cell, lit two cells with the same pod count for one
// half-finished OVS rollout.
func TestCollectServicesLeavesTheSwitchingLayerToTheNetworkView(t *testing.T) {
	svcs, _ := CollectServices(withWorkloads(
		wl("openstack", "keystone", "api", "Deployment", 3, 3, false),
		wl("openstack", "nova", "compute", "DaemonSet", 5, 5, false),
		wl("openstack", "ovn", "ovn-controller", "DaemonSet", 5, 2, false),
		wl("openstack", "openvswitch", "server", "DaemonSet", 5, 1, true),
	), "openstack")

	for _, svc := range svcs.Items {
		if networkOwned[svc.Name] {
			t.Errorf("%q is reported by both the cloud and network frames", svc.Name)
		}
	}
	if len(svcs.Items) != 2 {
		t.Errorf("got %d services, want 2 (keystone, nova)", len(svcs.Items))
	}
	// And the banner must not inherit the switching layer's backlog.
	if !svcs.Converged() {
		t.Errorf("the cloud reports as rolling because of workloads it does not own: %+v", svcs.Items)
	}
	if svcs.NeedsOperator() {
		t.Error("the cloud asks for an operator on Open vSwitch's behalf")
	}
}
