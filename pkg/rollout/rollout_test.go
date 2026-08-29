package rollout

import (
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

func putKCPs(s *store.Store, items ...model.KubeadmControlPlane) {
	s.Put(model.KeyMgmtKCPs, model.Snapshot[model.KubeadmControlPlane]{Items: items, UpdatedAt: time.Now()})
}

func putMDs(s *store.Store, items ...model.MachineDeployment) {
	s.Put(model.KeyMgmtMachineDeployments, model.Snapshot[model.MachineDeployment]{Items: items, UpdatedAt: time.Now()})
}

func TestDetect_SteadyState(t *testing.T) {
	s := store.New()
	putKCPs(s, model.KubeadmControlPlane{
		Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 3,
	})
	putMDs(s, model.MachineDeployment{
		Namespace: "capi", Name: "workers", DesiredReplicas: 5, UpToDateReplicas: 5,
	})

	got := Detect(s, "")
	if got.Active {
		t.Error("a fully up-to-date cluster should not be rolling")
	}
	if len(got.Rolling) != 0 {
		t.Errorf("Rolling: got %v want empty", got.Rolling)
	}
}

func TestDetect_ControlPlaneRolling(t *testing.T) {
	s := store.New()
	putKCPs(s, model.KubeadmControlPlane{
		Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 1,
	})

	got := Detect(s, "")
	if !got.Active {
		t.Fatal("expected Active")
	}
	if got.Asserted {
		t.Error("observed state should not be reported as asserted")
	}
	if len(got.Rolling) != 1 || !strings.Contains(got.Rolling[0], "capi/cp") {
		t.Errorf("Rolling: got %v", got.Rolling)
	}
	if !strings.Contains(got.Rolling[0], "KubeadmControlPlane") {
		t.Errorf("Rolling entry should name the kind: %q", got.Rolling[0])
	}
}

func TestDetect_MachineDeploymentRolling(t *testing.T) {
	s := store.New()
	putMDs(s, model.MachineDeployment{
		Namespace: "capi", Name: "workers", DesiredReplicas: 5, UpToDateReplicas: 2,
	})
	got := Detect(s, "")
	if !got.Active {
		t.Fatal("expected Active")
	}
	if len(got.Rolling) != 1 || !strings.Contains(got.Rolling[0], "MachineDeployment") {
		t.Errorf("Rolling: got %v", got.Rolling)
	}
}

// An operator-stated target version activates the mode before the controllers
// have replaced anything — the gap between editing a version and the first
// Machine going down is when a watcher is most wanted.
func TestDetect_AssertedByTargetVersion(t *testing.T) {
	s := store.New()
	putKCPs(s, model.KubeadmControlPlane{
		Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 3,
	})

	got := Detect(s, "v1.33.0")
	if !got.Active {
		t.Fatal("a target version should activate rollout mode")
	}
	if !got.Asserted {
		t.Error("Asserted should be true when the signal came from the operator")
	}
	if got.TargetVersion != "v1.33.0" {
		t.Errorf("TargetVersion: got %q", got.TargetVersion)
	}
	if len(got.Rolling) != 0 {
		t.Errorf("nothing is observably rolling: got %v", got.Rolling)
	}
}

// Observed state takes precedence in reporting: if something really is rolling,
// the state is not merely asserted.
func TestDetect_ObservedBeatsAsserted(t *testing.T) {
	s := store.New()
	putKCPs(s, model.KubeadmControlPlane{
		Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 1,
	})
	got := Detect(s, "v1.33.0")
	if !got.Active {
		t.Fatal("expected Active")
	}
	if got.Asserted {
		t.Error("with an observed rollout, Asserted should be false")
	}
}

// Before caches warm there is no data. The honest answer is "not rolling", not a
// steady state we never observed — and a target version still activates.
func TestDetect_EmptyStore(t *testing.T) {
	s := store.New()

	got := Detect(s, "")
	if got.Active {
		t.Error("an empty store should not report a rollout")
	}

	got = Detect(s, "v1.33.0")
	if !got.Active || !got.Asserted {
		t.Errorf("a target version should still activate: %+v", got)
	}
}

// An error snapshot must not be read as a steady state.
func TestDetect_ErrorSnapshotIsNotSteadyState(t *testing.T) {
	s := store.New()
	s.Put(model.KeyMgmtKCPs, model.ErrorSnapshot(errFake{}))

	got := Detect(s, "")
	if got.Active {
		t.Error("an unreadable snapshot should not report a rollout")
	}
	if len(got.Rolling) != 0 {
		t.Errorf("Rolling: got %v want empty", got.Rolling)
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

// Zero desired replicas is a scaled-to-zero pool, not a rollout.
func TestDetect_ZeroDesiredIsNotRolling(t *testing.T) {
	s := store.New()
	putMDs(s, model.MachineDeployment{
		Namespace: "capi", Name: "scaled-to-zero", DesiredReplicas: 0, UpToDateReplicas: 0,
	})
	if Detect(s, "").Active {
		t.Error("a pool with zero desired replicas should not report a rollout")
	}
}

func TestDetect_RollingIsSorted(t *testing.T) {
	s := store.New()
	putKCPs(s,
		model.KubeadmControlPlane{Namespace: "z-ns", Name: "cp", DesiredReplicas: 1},
		model.KubeadmControlPlane{Namespace: "a-ns", Name: "cp", DesiredReplicas: 1},
	)
	putMDs(s, model.MachineDeployment{Namespace: "m-ns", Name: "md", DesiredReplicas: 1})

	got := Detect(s, "").Rolling
	if len(got) != 3 {
		t.Fatalf("Rolling: got %v want 3 entries", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("Rolling is not sorted: %v", got)
			break
		}
	}
}

func TestActive(t *testing.T) {
	s := store.New()
	if Active(s, "") {
		t.Error("empty store should be inactive")
	}
	putKCPs(s, model.KubeadmControlPlane{Namespace: "capi", Name: "cp", DesiredReplicas: 3, UpToDateReplicas: 1})
	if !Active(s, "") {
		t.Error("expected active")
	}
}
