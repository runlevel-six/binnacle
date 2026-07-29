// Package rollout reports whether a Cluster API rollout is in progress, which
// drives the dashboard's mode-aware panes.
//
// The signal is derived from the cluster rather than declared by the operator.
// Cluster API reports an up-to-date replica count on each control plane and
// machine deployment, so a rollout is exactly the state where some replica is
// not yet up to date. An operator may also assert a rollout with a target
// version, which is honored before the controllers have started rolling
// anything — the interval between editing a version and the first Machine being
// replaced is precisely when a watcher is most wanted.
package rollout

import (
	"sort"

	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

// State is what the dashboard needs to know about a rollout.
type State struct {
	// Active reports whether rollout-flavored panes should be shown.
	Active bool
	// Asserted reports that Active came from an operator-supplied target
	// version rather than from observed cluster state.
	Asserted bool
	// Rolling names the resources with replicas still to update, as
	// "namespace/name (kind)", sorted for stable display.
	Rolling []string
	// TargetVersion is the operator's stated goal, if any.
	TargetVersion string
}

// Detect derives rollout state from the store.
//
// A non-empty targetVersion makes the state active regardless of observed
// counts. Otherwise any control plane or machine deployment with replicas left to
// update makes it active.
//
// Snapshots that have not loaded yet contribute nothing rather than being read as
// "not rolling". Before caches warm, the honest answer is that we do not know,
// and reporting a steady state we have not observed would be worse than
// reporting none.
func Detect(s *store.Store, targetVersion string) State {
	st := State{TargetVersion: targetVersion}

	if kcps, ok := store.Get[model.Snapshot[model.KubeadmControlPlane]](s, model.KeyMgmtKCPs); ok {
		for _, k := range kcps.Items {
			if k.Rolling() {
				st.Rolling = append(st.Rolling, k.Namespace+"/"+k.Name+" (KubeadmControlPlane)")
			}
		}
	}
	if mds, ok := store.Get[model.Snapshot[model.MachineDeployment]](s, model.KeyMgmtMachineDeployments); ok {
		for _, m := range mds.Items {
			if m.Rolling() {
				st.Rolling = append(st.Rolling, m.Namespace+"/"+m.Name+" (MachineDeployment)")
			}
		}
	}
	sort.Strings(st.Rolling)

	switch {
	case len(st.Rolling) > 0:
		st.Active = true
	case targetVersion != "":
		st.Active = true
		st.Asserted = true
	}
	return st
}

// Active is the boolean form of [Detect], for callers that only need the mode.
func Active(s *store.Store, targetVersion string) bool {
	return Detect(s, targetVersion).Active
}
