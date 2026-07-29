// Package model holds the render-ready snapshot types that data sources publish
// and panes consume.
//
// Nothing downstream of a source touches a Kubernetes type. Sources project API
// objects into these structs once; panes then read plain fields. That keeps
// client-go out of the render path and makes every pane testable from literals.
//
// Snapshot types carry only the fields the UI needs. Extend a type here rather
// than passing raw objects further down the chain.
package model

import "time"

// Datastore keys. Sources own the key they publish under, and the type behind
// each key is fixed by the constant's comment. Declared centrally so a source
// and a pane cannot silently disagree.
//
// The "mgmt/" and "workload/" prefixes name the cluster the data came from,
// using upstream Cluster API's vocabulary.
const (
	// KeyMgmtClusters holds Snapshot[Cluster].
	KeyMgmtClusters = "mgmt/clusters"
	// KeyMgmtKCPs holds Snapshot[KubeadmControlPlane].
	KeyMgmtKCPs = "mgmt/kubeadmcontrolplanes"
	// KeyMgmtMachineDeployments holds Snapshot[MachineDeployment].
	KeyMgmtMachineDeployments = "mgmt/machinedeployments"
	// KeyMgmtMachines holds Snapshot[Machine].
	KeyMgmtMachines = "mgmt/machines"
	// KeyMgmtEvents holds Snapshot[Event].
	KeyMgmtEvents = "mgmt/events"

	// KeyMgmtMetal3Clusters holds Snapshot[Metal3Cluster].
	KeyMgmtMetal3Clusters = "mgmt/metal3clusters"
	// KeyMgmtMetal3Machines holds Snapshot[Metal3Machine].
	KeyMgmtMetal3Machines = "mgmt/metal3machines"
	// KeyMgmtBareMetalHosts holds Snapshot[BareMetalHost].
	KeyMgmtBareMetalHosts = "mgmt/baremetalhosts"

	// KeyWorkloadNodes holds Snapshot[Node].
	KeyWorkloadNodes = "workload/nodes"
	// KeyWorkloadPods holds Snapshot[Pod].
	KeyWorkloadPods = "workload/pods"
	// KeyWorkloadEvents holds Snapshot[Event].
	KeyWorkloadEvents = "workload/events"
	// KeyWorkloadWorkloads holds Snapshot[Workload] — Deployments,
	// StatefulSets and DaemonSets normalised into one list.
	KeyWorkloadWorkloads = "workload/workloads"
)

// Snapshot wraps a list of T published by a source.
//
// Err is set on a failure such as a missing CRD or a denied permission. Items
// may then be stale or empty, and a pane should surface the error rather than
// rendering an empty state that looks like "nothing is wrong".
type Snapshot[T any] struct {
	Items     []T
	UpdatedAt time.Time
	Err       error
	// Note is a human-readable status for a snapshot that is not wrong, just not
	// ready: a cache still filling, a source still connecting. Panes render it in
	// place of an empty table, so "nothing yet, and here is why" is distinguishable
	// from "nothing, full stop" — a pane that says "loading" for minutes with no
	// reason is indistinguishable from a broken one.
	//
	// A note never accompanies data: it is dropped as soon as there is something
	// real to show.
	Note string
}

// ErrorSnapshot carries an error for a key whose concrete item type the caller
// does not want to name.
//
// It is deliberately Snapshot[any]: a source reporting "the CRD is absent" often
// cannot construct the real element type at that point. Panes read their typed
// snapshot first and fall back to this shape to find an error, so a type-erased
// failure is still surfaced rather than silently dropped.
func ErrorSnapshot(err error) Snapshot[any] {
	return Snapshot[any]{UpdatedAt: time.Now(), Err: err}
}

// Condition is the common subset of every status-condition flavor used by
// Cluster API, Metal3 and core Kubernetes, unified so panes need not know which
// API group a condition came from.
type Condition struct {
	Type           string
	Status         string // "True" | "False" | "Unknown"
	Reason         string
	Message        string
	LastTransition time.Time
}

// ReplicaBucket is the replica tuple Cluster API reports for a control plane or
// a worker pool. Older API versions may leave some fields zero.
type ReplicaBucket struct {
	Desired   int32
	Current   int32
	Ready     int32
	Available int32
	UpToDate  int32
}

// Rolling reports whether this bucket has replicas still to update. It is the
// per-bucket form of the rollout signal.
func (r ReplicaBucket) Rolling() bool {
	return r.Desired > 0 && r.UpToDate < r.Desired
}

// Cluster is a cluster.x-k8s.io Cluster.
type Cluster struct {
	Namespace    string
	Name         string
	ClusterClass string // .spec.topology.class; empty for a non-topology cluster
	Phase        string
	Available    bool
	Paused       bool
	Version      string // .spec.topology.version; empty for a non-topology cluster
	ControlPlane ReplicaBucket
	Workers      ReplicaBucket
	Age          time.Duration
	Conditions   []Condition
}

// KubeadmControlPlane is a controlplane.cluster.x-k8s.io KubeadmControlPlane.
type KubeadmControlPlane struct {
	Namespace           string
	Name                string
	ClusterName         string
	Version             string
	DesiredReplicas     int32
	Replicas            int32
	UpToDateReplicas    int32
	ReadyReplicas       int32
	AvailableReplicas   int32
	UnavailableReplicas int32
	Available           bool
	Initialized         bool
	Paused              bool
	Age                 time.Duration
	Conditions          []Condition
}

// Rolling reports whether this control plane has replicas still to update.
func (k KubeadmControlPlane) Rolling() bool {
	return k.DesiredReplicas > 0 && k.UpToDateReplicas < k.DesiredReplicas
}

// MachineDeployment is a cluster.x-k8s.io MachineDeployment.
type MachineDeployment struct {
	Namespace         string
	Name              string
	ClusterName       string
	Phase             string
	Version           string
	DesiredReplicas   int32
	Replicas          int32
	UpToDateReplicas  int32
	ReadyReplicas     int32
	AvailableReplicas int32
	Available         bool
	Paused            bool
	Age               time.Duration
	Conditions        []Condition
}

// Rolling reports whether this deployment has replicas still to update.
func (m MachineDeployment) Rolling() bool {
	return m.DesiredReplicas > 0 && m.UpToDateReplicas < m.DesiredReplicas
}

// Machine is a cluster.x-k8s.io Machine. OwnerKind and OwnerName come from
// ownerReferences, typically a KubeadmControlPlane or a MachineSet.
type Machine struct {
	Namespace   string
	Name        string
	ClusterName string
	NodeName    string
	Phase       string
	Version     string
	ProviderID  string
	OwnerKind   string
	OwnerName   string
	// InfraKind and InfraName identify the provider-specific machine from
	// .spec.infrastructureRef, e.g. Metal3Machine. Keeping the reference
	// generic is what lets a non-Metal3 provider be added without changing
	// this type.
	InfraKind  string
	InfraName  string
	Age        time.Duration
	Conditions []Condition
}

// Metal3Cluster is an infrastructure.cluster.x-k8s.io Metal3Cluster.
type Metal3Cluster struct {
	Namespace    string
	Name         string
	ClusterName  string
	Ready        bool
	EndpointHost string
	EndpointPort int32
	ErrorMessage string
	Age          time.Duration
	Conditions   []Condition
}

// Metal3Machine is an infrastructure.cluster.x-k8s.io Metal3Machine.
type Metal3Machine struct {
	Namespace  string
	Name       string
	Ready      bool
	ProviderID string
	// BMHNamespace and BMHName identify the BareMetalHost this machine
	// consumes, from the metal3.io/BareMetalHost annotation. The namespace is
	// kept because a host need not live alongside the machine that consumes it,
	// and dropping it would silently mis-join same-named hosts.
	BMHNamespace string
	BMHName      string
	Age          time.Duration
	Conditions   []Condition
}

// BareMetalHost is a metal3.io BareMetalHost — the physical machine.
type BareMetalHost struct {
	Namespace         string
	Name              string
	State             string // .status.provisioning.state
	OperationalStatus string
	PoweredOn         bool
	Online            bool // .spec.online
	ConsumerKind      string
	ConsumerNamespace string
	ConsumerName      string
	ErrorMessage      string
	Age               time.Duration
}

// Node is a core/v1 Node.
type Node struct {
	Name             string
	Status           string // Ready | NotReady | Unknown
	Roles            []string
	Role             string // the primary role, per the profile's label keys
	Age              time.Duration
	Version          string // kubelet version
	InternalIP       string
	ExternalIP       string
	OSImage          string
	KernelVersion    string
	ContainerRuntime string

	// Cordoned mirrors .spec.unschedulable. Nodes pass through this state while
	// being drained for a rollout.
	Cordoned bool

	// Pressure conditions, each true when the kubelet reports it True. False
	// by default, which matches a healthy node.
	MemoryPressure bool
	DiskPressure   bool
	PIDPressure    bool
	NetworkUnavail bool

	// Allocatable capacity and the summed requests and limits of pods
	// scheduled here, computed by joining the pod list at projection time.
	// CPU is millicores; memory is bytes. Percentages are left to the caller,
	// since the raw numbers are what keep panes consistent with each other.
	AllocatableCPU    int64
	AllocatableMemory int64
	RequestedCPU      int64
	RequestedMemory   int64
	LimitsCPU         int64
	LimitsMemory      int64

	Labels map[string]string
}

// Ready reports whether the node's Ready condition is True.
func (n Node) Ready() bool { return n.Status == "Ready" }

// DisplayStatus renders status and cordon state as one token.
//
// "Cordoned" is used rather than Kubernetes' own "SchedulingDisabled" because it
// is the word operators use, and during a rollout this column is read at a
// glance.
func (n Node) DisplayStatus() string {
	if n.Cordoned {
		if n.Ready() {
			return "Cordoned"
		}
		return n.Status + ",Cordoned"
	}
	return n.Status
}

// Workload is one apps/v1 Deployment, StatefulSet or DaemonSet, normalised so a
// pane can render all three in a single section.
type Workload struct {
	Namespace string
	Name      string
	Kind      string // "Deployment" | "StatefulSet" | "DaemonSet"
	Ready     int32
	Desired   int32
}

// Pod is a core/v1 Pod.
type Pod struct {
	Namespace  string
	Name       string
	ReadyReady int32 // numerator of "2/3"
	ReadyTotal int32 // denominator of "2/3"
	Status     string
	Restarts   int32
	Age        time.Duration
	IP         string
	Node       string
	Phase      string
	// IsHealthy is true when every container is ready, or the pod ran to
	// completion. It is the filter an unhealthy-pods view applies.
	IsHealthy bool
}

// Event is a core/v1 Event.
type Event struct {
	Namespace      string
	Type           string // Normal | Warning
	Reason         string
	ObjectKind     string
	ObjectName     string
	Message        string
	FirstTimestamp time.Time
	LastTimestamp  time.Time
	Count          int32
}
