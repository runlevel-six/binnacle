package capi

import (
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/internal/core/model"
)

func machine(ns, name, owner, ownerKind, infraName string) model.Machine {
	return model.Machine{
		Namespace: ns, Name: name,
		OwnerKind: ownerKind, OwnerName: owner,
		InfraKind: Metal3MachineKind, InfraName: infraName,
	}
}

func TestJoin_FullChainViaAnnotation(t *testing.T) {
	machines := []model.Machine{
		machine("capi", "prod-cp-1", "prod-cp", "KubeadmControlPlane", "prod-cp-1-m3m"),
	}
	m3ms := []model.Metal3Machine{
		{Namespace: "capi", Name: "prod-cp-1-m3m", Ready: true, BMHNamespace: "bmh", BMHName: "host-1"},
	}
	bmhs := []model.BareMetalHost{
		{Namespace: "bmh", Name: "host-1", State: "provisioned", PoweredOn: true},
	}

	rows := Join(machines, m3ms, bmhs, nil)
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if r.Metal3Machine == nil {
		t.Fatal("Metal3Machine not joined")
	}
	if r.BareMetalHost == nil {
		t.Fatal("BareMetalHost not joined")
	}
	if r.HostName() != "host-1" {
		t.Errorf("HostName: got %q want host-1", r.HostName())
	}
	if !r.Provisioned() {
		t.Error("a provisioned host should report Provisioned")
	}
	if !r.ControlPlane || r.Role != ControlPlaneRole {
		t.Errorf("control plane: ControlPlane=%v Role=%q", r.ControlPlane, r.Role)
	}
}

// The host must be found across namespaces. Dropping the namespace from the
// annotation would mis-join same-named hosts.
func TestJoin_CrossNamespaceHost(t *testing.T) {
	rows := Join(
		[]model.Machine{machine("capi", "m", "", "", "m3m")},
		[]model.Metal3Machine{{Namespace: "capi", Name: "m3m", BMHNamespace: "other-ns", BMHName: "host"}},
		[]model.BareMetalHost{
			{Namespace: "capi", Name: "host", State: "available"},       // decoy, same name
			{Namespace: "other-ns", Name: "host", State: "provisioned"}, // the real one
		},
		nil,
	)
	if rows[0].BareMetalHost == nil {
		t.Fatal("host not joined")
	}
	if got := rows[0].BareMetalHost.Namespace; got != "other-ns" {
		t.Errorf("joined the wrong namespace: got %q want other-ns", got)
	}
}

// The reverse path via spec.consumerRef covers a host that is claimed but whose
// annotation has not appeared, which is a real transient during provisioning.
func TestJoin_FallsBackToConsumerRef(t *testing.T) {
	rows := Join(
		[]model.Machine{machine("capi", "m", "", "", "m3m")},
		[]model.Metal3Machine{{Namespace: "capi", Name: "m3m"}}, // no annotation
		[]model.BareMetalHost{{
			Namespace: "bmh", Name: "host-9", State: "provisioning",
			ConsumerKind: Metal3MachineKind, ConsumerNamespace: "capi", ConsumerName: "m3m",
		}},
		nil,
	)
	if rows[0].BareMetalHost == nil {
		t.Fatal("host not joined via consumerRef")
	}
	if rows[0].BareMetalHost.Name != "host-9" {
		t.Errorf("got %q want host-9", rows[0].BareMetalHost.Name)
	}
}

// A consumerRef with no namespace means the host's own namespace.
func TestJoin_ConsumerRefWithoutNamespace(t *testing.T) {
	rows := Join(
		[]model.Machine{machine("capi", "m", "", "", "m3m")},
		[]model.Metal3Machine{{Namespace: "capi", Name: "m3m"}},
		[]model.BareMetalHost{{
			Namespace: "capi", Name: "host", ConsumerKind: Metal3MachineKind, ConsumerName: "m3m",
		}},
		nil,
	)
	if rows[0].BareMetalHost == nil {
		t.Error("host not joined when consumerRef omits the namespace")
	}
}

// A consumerRef pointing at some other kind must not be treated as a claim.
func TestJoin_IgnoresNonMetal3ConsumerRef(t *testing.T) {
	rows := Join(
		[]model.Machine{machine("capi", "m", "", "", "m3m")},
		[]model.Metal3Machine{{Namespace: "capi", Name: "m3m"}},
		[]model.BareMetalHost{{
			Namespace: "capi", Name: "host", ConsumerKind: "SomethingElse", ConsumerName: "m3m",
		}},
		nil,
	)
	if rows[0].BareMetalHost != nil {
		t.Error("a non-Metal3Machine consumerRef should not join")
	}
}

// A Machine with no provider machine yet still gets a row. Dropping it would
// hide exactly the machine an operator is waiting on.
func TestJoin_PendingMachineStillAppears(t *testing.T) {
	rows := Join(
		[]model.Machine{{Namespace: "capi", Name: "pending", Phase: "Pending"}},
		nil, nil, nil,
	)
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	if rows[0].Metal3Machine != nil || rows[0].BareMetalHost != nil {
		t.Error("expected nil provider machine and host")
	}
	if rows[0].HostName() != "" {
		t.Errorf("HostName: got %q want empty", rows[0].HostName())
	}
	if rows[0].Provisioned() {
		t.Error("a row with no host must not report Provisioned")
	}
}

// A Machine on another infrastructure provider joins nothing but still appears —
// Cluster API on a non-Metal3 provider is a legitimate target.
func TestJoin_NonMetal3InfraRef(t *testing.T) {
	m := model.Machine{Namespace: "capi", Name: "m", InfraKind: "AWSMachine", InfraName: "aws-1"}
	rows := Join([]model.Machine{m}, nil, nil, nil)
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	if rows[0].Metal3Machine != nil {
		t.Error("a non-Metal3 infra ref should not join a Metal3Machine")
	}
}

func TestJoin_RoleFunc(t *testing.T) {
	roleFn := func(m model.Machine) string {
		if strings.Contains(m.OwnerName, "workers") {
			return "compute"
		}
		return ""
	}
	rows := Join(
		[]model.Machine{
			machine("capi", "w-1", "prod-workers-abc", "MachineSet", "w-1-m3m"),
			machine("capi", "cp-1", "prod-cp", "KubeadmControlPlane", "cp-1-m3m"),
		},
		nil, nil, roleFn,
	)

	byName := map[string]HostRow{}
	for _, r := range rows {
		byName[r.Machine.Name] = r
	}
	if got := byName["w-1"].Role; got != "compute" {
		t.Errorf("worker role: got %q want compute", got)
	}
	// Control-plane membership comes from the ownerReference, so the RoleFunc is
	// not consulted for it.
	if got := byName["cp-1"].Role; got != ControlPlaneRole {
		t.Errorf("control-plane role: got %q want %q", got, ControlPlaneRole)
	}
}

// Control plane first, since that is the rollout stage watched most closely.
func TestJoin_OrdersControlPlaneFirst(t *testing.T) {
	rows := Join(
		[]model.Machine{
			machine("capi", "a-worker", "ms", "MachineSet", ""),
			machine("capi", "z-cp", "cp", "KubeadmControlPlane", ""),
			machine("capi", "b-worker", "ms", "MachineSet", ""),
		},
		nil, nil, nil,
	)
	if rows[0].Machine.Name != "z-cp" {
		t.Errorf("first row: got %q want z-cp (control plane leads)", rows[0].Machine.Name)
	}
	// Workers keep a stable name order behind it.
	if rows[1].Machine.Name != "a-worker" || rows[2].Machine.Name != "b-worker" {
		t.Errorf("worker order: got %q,%q", rows[1].Machine.Name, rows[2].Machine.Name)
	}
}

func TestJoin_StableAcrossCalls(t *testing.T) {
	machines := []model.Machine{
		machine("ns-b", "m", "ms", "MachineSet", ""),
		machine("ns-a", "m", "ms", "MachineSet", ""),
	}
	first := Join(machines, nil, nil, nil)
	for range 20 {
		got := Join(machines, nil, nil, nil)
		for i := range got {
			if got[i].Machine.Namespace != first[i].Machine.Namespace {
				t.Fatal("Join is not deterministic")
			}
		}
	}
	if first[0].Machine.Namespace != "ns-a" {
		t.Errorf("namespace should break ties: got %q", first[0].Machine.Namespace)
	}
}

func TestJoin_Empty(t *testing.T) {
	if rows := Join(nil, nil, nil, nil); len(rows) != 0 {
		t.Errorf("got %d rows want 0", len(rows))
	}
}

// --- unclaimed hosts ------------------------------------------------------

func TestUnclaimedHosts(t *testing.T) {
	bmhs := []model.BareMetalHost{
		{Namespace: "bmh", Name: "host-1", State: "provisioned"},
		{Namespace: "bmh", Name: "host-2", State: "available"},
		{Namespace: "bmh", Name: "host-3", State: "inspecting"},
	}
	m3ms := []model.Metal3Machine{{Namespace: "capi", Name: "m3m", BMHNamespace: "bmh", BMHName: "host-1"}}

	got := UnclaimedHosts(m3ms, bmhs)
	if len(got) != 2 {
		t.Fatalf("unclaimed: got %d want 2", len(got))
	}
	var names []string
	for _, b := range got {
		names = append(names, b.Name)
	}
	if strings.Join(names, ",") != "host-2,host-3" {
		t.Errorf("got %v want [host-2 host-3]", names)
	}
}

// Same-named hosts in different namespaces are distinct, so claiming one must
// not hide the other.
func TestUnclaimedHosts_NamespaceAware(t *testing.T) {
	bmhs := []model.BareMetalHost{
		{Namespace: "ns-a", Name: "host"},
		{Namespace: "ns-b", Name: "host"},
	}
	m3ms := []model.Metal3Machine{{Namespace: "capi", Name: "m3m", BMHNamespace: "ns-a", BMHName: "host"}}
	got := UnclaimedHosts(m3ms, bmhs)
	if len(got) != 1 || got[0].Namespace != "ns-b" {
		t.Errorf("got %+v want just ns-b/host", got)
	}
}

func TestUnclaimedHosts_NoRows(t *testing.T) {
	bmhs := []model.BareMetalHost{{Namespace: "bmh", Name: "host-1"}}
	if got := UnclaimedHosts(nil, bmhs); len(got) != 1 {
		t.Errorf("with no rows every host is unclaimed: got %d want 1", len(got))
	}
}
