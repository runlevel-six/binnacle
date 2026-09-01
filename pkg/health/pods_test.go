package health

import (
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

func TestNeedsAttention(t *testing.T) {
	tests := []struct {
		name string
		pod  model.Pod
		want bool
	}{{
		name: "a ready pod is not unhealthy at all",
		pod:  model.Pod{Status: "Running", IsHealthy: true, Age: time.Second},
	}, {
		// The case this exists for: a vulnerability scanner creating pods
		// continuously kept the cluster's pod cell permanently amber.
		name: "a pod a second into being created is only starting",
		pod:  model.Pod{Status: "ContainerCreating", Age: time.Second},
	}, {
		name: "so is one still initializing",
		pod:  model.Pod{Status: "Init:0/3", Age: 5 * time.Second},
	}, {
		name: "and one the scheduler has not placed yet",
		pod:  model.Pod{Status: "Pending", Age: 2 * time.Second},
	}, {
		name: "a pod still creating after the grace period needs looking at",
		pod:  model.Pod{Status: "ContainerCreating", Age: 2 * time.Minute},
		want: true,
	}, {
		name: "an init container stuck for nineteen days certainly does",
		pod:  model.Pod{Status: "Init:0/3", Age: 19 * 24 * time.Hour},
		want: true,
	}, {
		// Init:<reason> wears the same prefix as Init:N/M, and a pod broken
		// during startup is not a pod starting.
		name: "a failed init container is not excused by being new",
		pod:  model.Pod{Status: "Init:Error", Age: time.Second},
		want: true,
	}, {
		name: "nor is one crash-looping in init",
		pod:  model.Pod{Status: "Init:CrashLoopBackOff", Age: time.Second},
		want: true,
	}, {
		// A restart means the pod already ran and stopped, whatever it is doing
		// now — a fast crash loop can be seconds old and creating a container.
		name: "a restart disqualifies a pod from the grace period",
		pod:  model.Pod{Status: "ContainerCreating", Age: time.Second, Restarts: 3},
		want: true,
	}, {
		name: "a crash loop is never just starting",
		pod:  model.Pod{Status: "CrashLoopBackOff", Age: time.Second},
		want: true,
	}, {
		name: "neither is an image that cannot be pulled",
		pod:  model.Pod{Status: "ImagePullBackOff", Age: time.Second},
		want: true,
	}, {
		name: "the boundary belongs to the older pod",
		pod:  model.Pod{Status: "ContainerCreating", Age: PodGrace},
		want: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsAttention(tc.pod); got != tc.want {
				t.Errorf("NeedsAttention(%+v) = %v, want %v", tc.pod, got, tc.want)
			}
		})
	}
}

// The cell is what a reader actually sees, and a cluster that creates pods
// continuously must be able to reach green.
func TestCellPods_StartingPodsDoNotHoldTheCellAmber(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{
		Items: []model.Pod{
			{Namespace: "kube-system", Name: "cilium-czwz7", Status: "Running", IsHealthy: true},
			// Three scan jobs a second old, which is the steady state on a
			// cluster running a vulnerability scanner.
			{Namespace: "trivy-system", Name: "scan-1", Status: "ContainerCreating", Age: time.Second},
			{Namespace: "trivy-system", Name: "scan-2", Status: "ContainerCreating", Age: time.Second},
			{Namespace: "trivy-system", Name: "scan-3", Status: "ContainerCreating"},
		},
		UpdatedAt: time.Now(),
	})

	cell, ok := cellPods(s, profile.NodeRoles{})
	if !ok {
		t.Fatal("no pods cell")
	}
	if cell.Status != StatusOK {
		t.Errorf("status = %v (%q), want OK", cell.Status, cell.Detail)
	}

	// And a real problem in the same fleet still warns, with a count that does
	// not include the scan jobs.
	s.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{
		Items: []model.Pod{
			{Namespace: "example-system", Name: "api-0", Status: "Init:0/3", Age: 19 * 24 * time.Hour},
			{Namespace: "trivy-system", Name: "scan-1", Status: "ContainerCreating", Age: time.Second},
		},
		UpdatedAt: time.Now(),
	})
	cell, _ = cellPods(s, profile.NodeRoles{})
	if cell.Status != StatusWarn {
		t.Errorf("status = %v, want Warn", cell.Status)
	}
	if cell.Detail != "1 unhealthy" {
		t.Errorf("detail = %q, want \"1 unhealthy\"", cell.Detail)
	}
}

// A fleet view scopes its hosts pane to the cluster that owns the hardware, and
// its cell has to agree. The datacenter-wide snapshot behind cellHosts would
// otherwise report another cluster's failed host on every cluster's page, with
// no row anywhere to explain it.
func TestHostsCell_JudgesOnlyTheHostsGiven(t *testing.T) {
	mine := []model.BareMetalHost{
		{Name: "a03-17-controller", State: "provisioned", OperationalStatus: "OK"},
		{Name: "a03-18-controller", State: "provisioned", OperationalStatus: "OK"},
	}
	theirs := model.BareMetalHost{
		Name: "a03-22-compute", State: "deprovisioning",
		OperationalStatus: "error", ErrorMessage: "Cleaning failed",
	}

	cell, ok := HostsCell(mine)
	if !ok {
		t.Fatal("no hosts cell")
	}
	if cell.Status != StatusOK {
		t.Errorf("status = %v (%q), want OK", cell.Status, cell.Detail)
	}

	// The same failure, in this cluster's own hosts, is still reported.
	cell, _ = HostsCell(append(mine, theirs))
	if cell.Status != StatusErr {
		t.Errorf("status = %v, want Err", cell.Status)
	}
	if cell.Detail != "1 errored" {
		t.Errorf("detail = %q, want \"1 errored\"", cell.Detail)
	}
	if cell.Name != CellNameHosts {
		t.Errorf("name = %q, want %q", cell.Name, CellNameHosts)
	}
}

// No hosts is not the same as no problem: a cluster whose hosts have not been
// read yet must not render as healthy hardware.
func TestHostsCell_EmptyIsLoading(t *testing.T) {
	cell, ok := HostsCell(nil)
	if !ok {
		t.Fatal("no hosts cell")
	}
	if cell.Status != StatusLoading {
		t.Errorf("status = %v, want Loading", cell.Status)
	}
}
