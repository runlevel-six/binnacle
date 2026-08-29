package model

import (
	"errors"
	"testing"
)

// Rolling is the rollout signal the whole mode-aware dashboard turns on, so its
// edge cases are worth pinning down in this package rather than only indirectly.
func TestReplicaBucketRolling(t *testing.T) {
	tests := []struct {
		name    string
		bucket  ReplicaBucket
		rolling bool
	}{
		{"fully up to date", ReplicaBucket{Desired: 3, UpToDate: 3}, false},
		{"partially rolled", ReplicaBucket{Desired: 3, UpToDate: 1}, true},
		{"none rolled yet", ReplicaBucket{Desired: 3, UpToDate: 0}, true},
		// A pool scaled to zero is not mid-rollout; without the Desired > 0
		// guard, 0 < 0 would be false anyway, but an explicitly empty pool
		// should never read as rolling.
		{"scaled to zero", ReplicaBucket{Desired: 0, UpToDate: 0}, false},
		// More up to date than desired happens briefly while scaling down.
		{"scaling down", ReplicaBucket{Desired: 2, UpToDate: 3}, false},
		{"zero value", ReplicaBucket{}, false},
	}
	for _, tc := range tests {
		if got := tc.bucket.Rolling(); got != tc.rolling {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.rolling)
		}
	}
}

func TestKubeadmControlPlaneRolling(t *testing.T) {
	if !(KubeadmControlPlane{DesiredReplicas: 3, UpToDateReplicas: 1}).Rolling() {
		t.Error("1 of 3 up to date should be rolling")
	}
	if (KubeadmControlPlane{DesiredReplicas: 3, UpToDateReplicas: 3}).Rolling() {
		t.Error("3 of 3 should not be rolling")
	}
	if (KubeadmControlPlane{}).Rolling() {
		t.Error("the zero value should not be rolling")
	}
}

func TestMachineDeploymentRolling(t *testing.T) {
	if !(MachineDeployment{DesiredReplicas: 5, UpToDateReplicas: 4}).Rolling() {
		t.Error("4 of 5 up to date should be rolling")
	}
	if (MachineDeployment{DesiredReplicas: 5, UpToDateReplicas: 5}).Rolling() {
		t.Error("5 of 5 should not be rolling")
	}
	if (MachineDeployment{}).Rolling() {
		t.Error("the zero value should not be rolling")
	}
}

func TestNodeReady(t *testing.T) {
	if !(Node{Status: "Ready"}).Ready() {
		t.Error("Ready should report ready")
	}
	for _, s := range []string{"NotReady", "Unknown", ""} {
		if (Node{Status: s}).Ready() {
			t.Errorf("%q should not report ready", s)
		}
	}
}

// Cordon state is reported alongside readiness rather than replacing it, so a
// drained-but-healthy node is distinguishable from a broken one.
func TestNodeDisplayStatus(t *testing.T) {
	tests := []struct {
		node Node
		want string
	}{
		{Node{Status: "Ready"}, "Ready"},
		{Node{Status: "Ready", Cordoned: true}, "Cordoned"},
		{Node{Status: "NotReady"}, "NotReady"},
		{Node{Status: "NotReady", Cordoned: true}, "NotReady,Cordoned"},
		{Node{Status: "Unknown", Cordoned: true}, "Unknown,Cordoned"},
	}
	for _, tc := range tests {
		if got := tc.node.DisplayStatus(); got != tc.want {
			t.Errorf("status=%q cordoned=%v: got %q want %q",
				tc.node.Status, tc.node.Cordoned, got, tc.want)
		}
	}
}

func TestErrorSnapshot(t *testing.T) {
	boom := errors.New("crd not installed")
	got := ErrorSnapshot(boom)

	if !errors.Is(got.Err, boom) {
		t.Errorf("Err: got %v want %v", got.Err, boom)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be stamped so a reader can tell how stale the failure is")
	}
	if len(got.Items) != 0 {
		t.Errorf("Items: got %v want empty", got.Items)
	}
}

// Keys are the contract between a source and a pane. Prefixes name the cluster
// the data came from, so a mix-up would silently read the wrong cluster.
func TestKeyPrefixes(t *testing.T) {
	mgmt := []string{
		KeyMgmtClusters, KeyMgmtKCPs, KeyMgmtMachineDeployments, KeyMgmtMachines,
		KeyMgmtEvents, KeyMgmtMetal3Clusters, KeyMgmtMetal3Machines, KeyMgmtBareMetalHosts,
	}
	workload := []string{
		KeyWorkloadNodes, KeyWorkloadPods, KeyWorkloadEvents, KeyWorkloadWorkloads,
	}

	seen := map[string]bool{}
	for _, k := range mgmt {
		if len(k) < 6 || k[:5] != "mgmt/" {
			t.Errorf("%q should carry the mgmt/ prefix", k)
		}
		if seen[k] {
			t.Errorf("duplicate key %q", k)
		}
		seen[k] = true
	}
	for _, k := range workload {
		if len(k) < 10 || k[:9] != "workload/" {
			t.Errorf("%q should carry the workload/ prefix", k)
		}
		if seen[k] {
			t.Errorf("duplicate key %q", k)
		}
		seen[k] = true
	}
}
