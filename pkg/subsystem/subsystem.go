// Package subsystem holds the vocabulary shared by sextant's optional
// subsystems: how much of one can currently be seen, and how far through an
// upgrade its workloads are.
//
// These two ideas are not Ceph's or Cilium's or OVN's. Every plugin reports a
// tier, because every plugin can be denied the exec permission its detail
// needs; and every plugin watches something that gets upgraded, which nothing
// in Kubernetes reports honestly on its own. Naming them once is what lets a
// consumer outside the terminal read any subsystem's state without knowing
// which subsystem it is.
package subsystem

// Tier is how much detail a plugin can currently provide.
type Tier int

const (
	// TierAbsent means the subsystem is not installed. The plugin contributes
	// nothing at all — no pane, no banner cell.
	TierAbsent Tier = iota
	// TierInformer means the subsystem is present but its detail commands cannot
	// be run, usually for want of pods/exec. The plugin renders what the API
	// alone reveals.
	TierInformer
	// TierFull means detail commands work.
	TierFull
)

// String returns the tier's name, for diagnostics.
func (t Tier) String() string {
	switch t {
	case TierAbsent:
		return "absent"
	case TierInformer:
		return "informer-only"
	case TierFull:
		return "full"
	}
	return "?"
}

// Rollout is one workload's progress toward its own current pod template.
//
// Every plugin here watches something that gets upgraded, and none of them could
// previously say whether an upgrade had finished. Readiness does not answer it: a
// DaemonSet whose spec was updated an hour ago reports every pod Ready while half
// of them still run the old image. Ready is about now; updated is about which
// version is running.
//
// # The state nothing else reports
//
// A workload carrying instances cannot be restarted at the controller's
// convenience. Open vSwitch holds every VM's networking on its host, so
// restarting its pod cuts those instances off until it returns. Charts that know
// this ship the DaemonSet with the OnDelete update strategy: the spec updates and
// Kubernetes then does nothing whatsoever until an operator drains each host and
// deletes its pod by hand.
//
// That state is invisible to every standard tool. `kubectl rollout status` refuses
// outright — "rollout status is only available for RollingUpdate strategy type" —
// and the workload reports no condition, no event and no complaint. It sits at 3
// of 57 updated, indefinitely, looking exactly like a healthy DaemonSet. A chart
// bumped and never finished looks identical on day one and week six.
//
// So [Rollout.Manual] is its own state rather than a flavor of "rolling". A
// rolling workload will be fine if left alone. A manual one will not.
type Rollout struct {
	// Desired, Updated and Ready are pod counts. Updated is the number running
	// the workload's current pod template, which the controllers maintain for
	// every update strategy — including OnDelete, where nothing else reports
	// progress at all.
	Desired, Updated, Ready int32
	// Manual reports that no controller will finish this on its own.
	Manual bool
	// StaleNodes names the nodes still running a superseded pod, sorted. Only
	// node-pinned workloads populate it; see [Client.StaleNodes].
	StaleNodes []string
}

// Stale is how many pods are still running a superseded template.
func (r Rollout) Stale() int32 { return max(r.Desired-r.Updated, 0) }

// Converged reports whether every pod runs the current template.
//
// A workload with no desired pods is converged: there is nothing left to update.
// That is the honest answer for the empty DaemonSets a per-node-configuration
// split leaves behind when a node is relabeled.
func (r Rollout) Converged() bool { return r.Stale() == 0 }

// Known reports whether this rollout describes anything at all, so a caller can
// tell "nothing to update" from "never looked".
func (r Rollout) Known() bool { return r.Desired > 0 }

// Add folds another workload's rollout into this one.
//
// Aggregation is needed because a single logical component is often several
// workloads. OpenStack-Helm splits one service across a DaemonSet per distinct
// node configuration, naming each by a hash of it — a cluster can carry four
// `openvswitch-server-*` DaemonSets, two of them empty leftovers from nodes since
// relabeled. Reporting those as four rows fragments the one number an operator
// wants and pads it with meaningless zeroes.
//
// Manual is true only when *every* contributing workload is manual. A component
// served by a mix has a half that the controller is already replacing, and calling
// the whole thing manual would send someone hunting for pods to delete that are
// being deleted for them.
func (r Rollout) Add(other Rollout) Rollout {
	if !r.Known() {
		return other
	}
	if !other.Known() {
		return r
	}
	return Rollout{
		Desired:    r.Desired + other.Desired,
		Updated:    r.Updated + other.Updated,
		Ready:      r.Ready + other.Ready,
		Manual:     r.Manual && other.Manual,
		StaleNodes: append(append([]string(nil), r.StaleNodes...), other.StaleNodes...),
	}
}
