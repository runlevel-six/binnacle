// Package demo builds a dashboard's worth of invented cluster data.
//
// It exists for three reasons, in ascending order of how much they matter.
// Screenshots of a real cluster cannot be published — they carry hostnames,
// kubeconfig context names and address pools. A stranger with no Metal3 cluster
// should still be able to see what this tool does. And every rendering test in
// the tree exercises the core panes only, because the plugin panes have no data
// to render without a live subsystem behind them; a fixture is the only thing
// that puts them under test at all.
//
// # It replaces acquisition, never rendering
//
// A pane is a pure function of (snapshot, size, focus) and reads the store on
// every Render, so filling a store is the whole of what a demo has to do. The
// panes, profile, theme, layout and store are the real ones. Nothing here may
// grow into a second implementation of the dashboard — if it does, a demo
// screenshot stops being evidence about the real one.
//
// # Names
//
// Addresses come from 192.0.2.0/24 (RFC 5737 TEST-NET-1) and hostnames from the
// .example TLD (RFC 6761), both reserved for documentation, so nothing here can
// ever collide with a real deployment.
//
// # Determinism
//
// Two renders of the same fixture must be byte-identical, or every regenerated
// screenshot carries a spurious diff. Almost all of the model is relative
// already — [model.Node], [model.Pod] and the Cluster API types carry an Age
// duration rather than a timestamp. The exceptions are event timestamps and each
// snapshot's UpdatedAt, which are built as now-minus-age here rather than by
// freezing a clock: the render path's use of the real clock is correct, and a
// frozen one would be a second behavior to reason about.
package demo

import (
	"time"

	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// TargetVersion is the version the fixture's rollout is moving to. Exported so
// the demo's resolved config and its fixture cannot disagree about it — an
// asserted target that did not match the data would put the header into rollout
// mode over a cluster that was not rolling.
const TargetVersion = "v1.32.1"

// priorVersion is what the fixture is rolling *from*.
const priorVersion = "v1.32.0"

// ManagementContext and WorkloadContext are the invented kubeconfig contexts.
// They are deliberately two different clusters: the two-context header is the
// harder layout to get right and the one worth putting in a screenshot.
const (
	ManagementContext = "demo-management"
	WorkloadContext   = "demo-workload"
)

// Resolved returns the invented configuration the dashboard runs against.
//
// The profile is the real built-in default rather than a demo-specific one, so a
// screenshot shows what a newcomer actually gets on first run.
func Resolved(theme tui.Theme) config.Resolved {
	return config.Resolved{
		ManagementContext:    ManagementContext,
		WorkloadContext:      WorkloadContext,
		WorkloadIsManagement: false,
		Profile:              profile.Default(),
		TargetVersion:        TargetVersion,
		Theme:                theme,
	}
}

// Store returns a store pre-filled with the whole fixture, core and plugin
// alike, as though every watcher and source had just published.
func Store() *store.Store {
	s := store.New()
	now := time.Now()
	putCore(s, now)
	putPlugins(s, now)
	return s
}

// snap wraps items as a freshly published snapshot.
func snap[T any](now time.Time, items []T) model.Snapshot[T] {
	return model.Snapshot[T]{Items: items, UpdatedAt: now}
}

// putCore fills the keys the core panes read.
//
// The state is curated rather than healthy. A screenshot of an idle dashboard
// argues nothing; this one is a control-plane rollout in flight with the things
// that actually go wrong during one — a host that failed to provision, a node
// that has not come back, and workloads that noticed.
func putCore(s *store.Store, now time.Time) {
	s.Put(model.KeyMgmtClusters, snap(now, []model.Cluster{{
		Namespace: "demo", Name: "demo", Phase: "Provisioned",
		Available: true, Version: TargetVersion,
		ControlPlane: model.ReplicaBucket{Desired: 3, Current: 3, Ready: 3, Available: 3, UpToDate: 1},
		Workers:      model.ReplicaBucket{Desired: 3, Current: 3, Ready: 2, Available: 2, UpToDate: 3},
		Age:          168 * 24 * time.Hour,
	}}))

	s.Put(model.KeyMgmtKCPs, snap(now, []model.KubeadmControlPlane{{
		Namespace: "demo", Name: "demo-control-plane", ClusterName: "demo",
		Version: TargetVersion,
		// One of three replaced: mid-rollout, which is the state this tool is for.
		DesiredReplicas: 3, Replicas: 3, UpToDateReplicas: 1,
		ReadyReplicas: 3, AvailableReplicas: 3,
		Available: true, Initialized: true,
		Age: 168 * 24 * time.Hour,
	}}))

	s.Put(model.KeyMgmtMachineDeployments, snap(now, []model.MachineDeployment{{
		Namespace: "demo", Name: "demo-workers", ClusterName: "demo",
		Phase: "Running", Version: priorVersion,
		DesiredReplicas: 3, Replicas: 3, UpToDateReplicas: 3,
		ReadyReplicas: 2, AvailableReplicas: 2,
		Available: true,
		Age:       168 * 24 * time.Hour,
	}}))

	s.Put(model.KeyMgmtMachines, snap(now, []model.Machine{
		{
			Namespace: "demo", Name: "demo-control-plane-7fh2d", ClusterName: "demo",
			NodeName: "node-01.demo.example", Phase: "Running", Version: TargetVersion,
			OwnerKind: "KubeadmControlPlane", OwnerName: "demo-control-plane",
			InfraKind: "Metal3Machine", InfraName: "demo-control-plane-7fh2d",
			Age: 42 * time.Minute,
		},
		{
			// The one being replaced right now.
			Namespace: "demo", Name: "demo-control-plane-x9k4m", ClusterName: "demo",
			Phase: "Provisioning", Version: TargetVersion,
			OwnerKind: "KubeadmControlPlane", OwnerName: "demo-control-plane",
			InfraKind: "Metal3Machine", InfraName: "demo-control-plane-x9k4m",
			Age: 6 * time.Minute,
		},
		{
			Namespace: "demo", Name: "demo-control-plane-b3n8q", ClusterName: "demo",
			NodeName: "node-03.demo.example", Phase: "Running", Version: priorVersion,
			OwnerKind: "KubeadmControlPlane", OwnerName: "demo-control-plane",
			InfraKind: "Metal3Machine", InfraName: "demo-control-plane-b3n8q",
			Age: 168 * 24 * time.Hour,
		},
		{
			Namespace: "demo", Name: "demo-workers-5d9c7-lm2vp", ClusterName: "demo",
			NodeName: "node-04.demo.example", Phase: "Running", Version: priorVersion,
			OwnerKind: "MachineSet", OwnerName: "demo-workers-5d9c7",
			InfraKind: "Metal3Machine", InfraName: "demo-workers-5d9c7-lm2vp",
			Age: 168 * 24 * time.Hour,
		},
		{
			Namespace: "demo", Name: "demo-workers-5d9c7-qt6rk", ClusterName: "demo",
			NodeName: "node-05.demo.example", Phase: "Running", Version: priorVersion,
			OwnerKind: "MachineSet", OwnerName: "demo-workers-5d9c7",
			InfraKind: "Metal3Machine", InfraName: "demo-workers-5d9c7-qt6rk",
			Age: 168 * 24 * time.Hour,
		},
		{
			Namespace: "demo", Name: "demo-workers-5d9c7-w8xzc", ClusterName: "demo",
			NodeName: "node-06.demo.example", Phase: "Running", Version: priorVersion,
			OwnerKind: "MachineSet", OwnerName: "demo-workers-5d9c7",
			InfraKind: "Metal3Machine", InfraName: "demo-workers-5d9c7-w8xzc",
			Age: 168 * 24 * time.Hour,
		},
	}))

	s.Put(model.KeyMgmtMetal3Clusters, snap(now, []model.Metal3Cluster{{
		Namespace: "demo", Name: "demo", ClusterName: "demo", Ready: true,
		EndpointHost: "192.0.2.10", EndpointPort: 6443,
		Age: 168 * 24 * time.Hour,
	}}))

	// Metal3Machines are what join a Machine to the physical host it consumes.
	// Leaving them out is not a blank column — it is a HOST column of "unbound"
	// for every row, which is exactly the wrong thing for a bare-metal tool's
	// screenshot to show.
	s.Put(model.KeyMgmtMetal3Machines, snap(now, []model.Metal3Machine{
		m3("demo-control-plane-7fh2d", "node-01", true),
		m3("demo-control-plane-x9k4m", "node-07", false),
		m3("demo-control-plane-b3n8q", "node-03", true),
		m3("demo-workers-5d9c7-lm2vp", "node-04", true),
		m3("demo-workers-5d9c7-qt6rk", "node-05", true),
		m3("demo-workers-5d9c7-w8xzc", "node-06", true),
	}))

	s.Put(model.KeyMgmtBareMetalHosts, snap(now, []model.BareMetalHost{
		host("node-01", "provisioned", "demo-control-plane-7fh2d"),
		host("node-02", "available", ""),
		host("node-03", "provisioned", "demo-control-plane-b3n8q"),
		host("node-04", "provisioned", "demo-workers-5d9c7-lm2vp"),
		host("node-05", "provisioned", "demo-workers-5d9c7-qt6rk"),
		host("node-06", "provisioned", "demo-workers-5d9c7-w8xzc"),
		{
			// The failure a rollout most often stalls on, and the reason the
			// Hosts banner cell is red rather than amber.
			Namespace: "demo", Name: "node-07", State: "provisioning",
			OperationalStatus: "error", PoweredOn: false, Online: true,
			ConsumerKind: "Metal3Machine", ConsumerNamespace: "demo",
			ConsumerName: "demo-control-plane-x9k4m",
			ErrorMessage: "Introspection timed out after 30m0s",
			Age:          168 * 24 * time.Hour,
		},
	}))

	s.Put(model.KeyWorkloadNodes, snap(now, []model.Node{
		node("node-01", "control-plane", TargetVersion, "192.0.2.11", 42*time.Minute, nodeOpts{}),
		node("node-03", "control-plane", priorVersion, "192.0.2.13", 168*24*time.Hour, nodeOpts{}),
		node("node-04", "worker", priorVersion, "192.0.2.14", 168*24*time.Hour, nodeOpts{}),
		// Drained ahead of its turn in the rollout: cordoned and Ready, which
		// must read as amber rather than as a fault.
		node("node-05", "worker", priorVersion, "192.0.2.15", 168*24*time.Hour, nodeOpts{cordoned: true}),
		// Has not come back. This is the red one.
		node("node-06", "worker", priorVersion, "192.0.2.16", 168*24*time.Hour, nodeOpts{notReady: true}),
	}))

	s.Put(model.KeyWorkloadPods, snap(now, []model.Pod{
		pod("kube-system", "coredns-6f8b4d9c7-2xk9v", 1, 1, "Running", 0, 168*24*time.Hour, "node-01.demo.example", true),
		pod("kube-system", "kube-apiserver-node-01", 1, 1, "Running", 0, 42*time.Minute, "node-01.demo.example", true),
		pod("kube-system", "kube-apiserver-node-03", 1, 1, "Running", 0, 168*24*time.Hour, "node-03.demo.example", true),
		// The pods that noticed node-06 going away.
		pod("monitoring", "prometheus-server-0", 0, 2, "Pending", 0, 11*time.Minute, "", false),
		pod("ingress", "ingress-nginx-controller-t4w7p", 0, 1, "Terminating", 0, 168*24*time.Hour, "node-06.demo.example", false),
		pod("storage", "csi-node-driver-9mq2f", 0, 3, "CrashLoopBackOff", 7, 168*24*time.Hour, "node-06.demo.example", false),
	}))

	s.Put(model.KeyWorkloadWorkloads, snap(now, []model.Workload{
		{Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Ready: 2, Desired: 2},
		{Namespace: "kube-system", Name: "kube-proxy", Kind: "DaemonSet", Ready: 4, Desired: 5},
		{Namespace: "ingress", Name: "ingress-nginx-controller", Kind: "Deployment", Ready: 1, Desired: 2},
		{Namespace: "monitoring", Name: "prometheus-server", Kind: "StatefulSet", Ready: 0, Desired: 1},
		{Namespace: "storage", Name: "csi-node-driver", Kind: "DaemonSet", Ready: 4, Desired: 5},
	}))

	s.Put(model.KeyMgmtEvents, snap(now, []model.Event{
		event(now, "Warning", "ProvisioningError", "BareMetalHost", "node-07",
			"Introspection timed out after 30m0s", 4*time.Minute, 3),
		event(now, "Normal", "DrainNode", "Machine", "demo-control-plane-x9k4m",
			"Draining node node-02.demo.example", 8*time.Minute, 1),
		event(now, "Normal", "MachineCreated", "MachineSet", "demo-control-plane",
			"Created machine demo-control-plane-x9k4m", 9*time.Minute, 1),
		event(now, "Normal", "RollingUpdate", "KubeadmControlPlane", "demo-control-plane",
			"Rolling 3 replicas to "+TargetVersion, 12*time.Minute, 1),
	}))

	s.Put(model.KeyWorkloadEvents, snap(now, []model.Event{
		event(now, "Warning", "BackOff", "Pod", "csi-node-driver-9mq2f",
			"Back-off restarting failed container", 2*time.Minute, 7),
		event(now, "Warning", "FailedScheduling", "Pod", "prometheus-server-0",
			"0/5 nodes are available: 1 node(s) had untolerated taint", 3*time.Minute, 11),
		event(now, "Normal", "NodeNotReady", "Node", "node-06.demo.example",
			"Node node-06.demo.example status is now: NodeNotReady", 14*time.Minute, 1),
	}))
}

func m3(name, bmh string, ready bool) model.Metal3Machine {
	return model.Metal3Machine{
		Namespace: "demo", Name: name, Ready: ready,
		BMHNamespace: "demo", BMHName: bmh,
		Age: 168 * 24 * time.Hour,
	}
}

// host builds a provisioned BareMetalHost. Every host in the fixture is the same
// age — a fleet racked at once, which is what bare metal usually is.
func host(name, state, consumer string) model.BareMetalHost {
	const age = 168 * 24 * time.Hour
	h := model.BareMetalHost{
		Namespace: "demo", Name: name, State: state,
		OperationalStatus: "OK", PoweredOn: true, Online: true, Age: age,
	}
	if consumer != "" {
		h.ConsumerKind = "Metal3Machine"
		h.ConsumerNamespace = "demo"
		h.ConsumerName = consumer
	}
	return h
}

// nodeOpts keeps the node helper readable at its call sites — a row of bare
// booleans would not say which is which.
type nodeOpts struct {
	cordoned bool
	notReady bool
}

func node(short, role, version, ip string, age time.Duration, o nodeOpts) model.Node {
	status := "Ready"
	if o.notReady {
		status = "NotReady"
	}
	roles := []string{role}
	n := model.Node{
		Name: short + ".demo.example", Status: status, Roles: roles, Role: role,
		Age: age, Version: version, InternalIP: ip,
		OSImage:          "Ubuntu 24.04.1 LTS",
		KernelVersion:    "6.8.0-51-generic",
		ContainerRuntime: "containerd://1.7.24",
		Cordoned:         o.cordoned,
		AllocatableCPU:   32000, AllocatableMemory: 128 * 1 << 30,
		RequestedCPU: 9600, RequestedMemory: 42 * 1 << 30,
		LimitsCPU: 24000, LimitsMemory: 96 * 1 << 30,
		Labels: map[string]string{"node-role.kubernetes.io/" + role: ""},
	}
	return n
}

func pod(ns, name string, ready, total int32, status string, restarts int32,
	age time.Duration, nodeName string, healthy bool) model.Pod {
	return model.Pod{
		Namespace: ns, Name: name, ReadyReady: ready, ReadyTotal: total,
		Status: status, Restarts: restarts, Age: age, Node: nodeName,
		Phase: status, IsHealthy: healthy,
	}
}

// event builds an event that happened age ago. Both timestamps are derived from
// the caller's now, so the age column renders the same on every run.
func event(now time.Time, typ, reason, kind, name, msg string,
	age time.Duration, count int32) model.Event {
	last := now.Add(-age)
	return model.Event{
		Namespace: "demo", Type: typ, Reason: reason,
		ObjectKind: kind, ObjectName: name, Message: msg,
		FirstTimestamp: last.Add(-time.Duration(count) * time.Minute),
		LastTimestamp:  last, Count: count,
	}
}
