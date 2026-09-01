package health

import (
	"fmt"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

// CoreCells builds the health strip's core cells from the store: Cluster API
// rollout progress, Machines, BareMetalHosts, nodes and pods, in that order.
//
// Order is significance, and a consumer short of room drops from the right. A
// cell whose snapshot has not arrived, or arrived carrying an error, is omitted
// entirely rather than reported healthy — see [StatusLoading] for the case
// where the snapshot is present but empty.
//
// Plugin cells are not included, because a plugin registry cannot be named from
// here without an import cycle. Append them:
//
//	cells := append(health.CoreCells(s, roles), reg.BannerCells(s)...)
func CoreCells(s *store.Store, roles profile.NodeRoles) []Cell {
	var cells []Cell
	for _, build := range []func(*store.Store, profile.NodeRoles) (Cell, bool){
		cellRollout, cellMachines, cellHosts, cellNodes, cellPods,
	} {
		if c, ok := build(s, roles); ok {
			cells = append(cells, c)
		}
	}
	return cells
}

// cellRollout summarizes Cluster API rollout progress.
func cellRollout(s *store.Store, roles profile.NodeRoles) (Cell, bool) {
	kcps, ok := store.Get[model.Snapshot[model.KubeadmControlPlane]](s, model.KeyMgmtKCPs)
	mds, mdOK := store.Get[model.Snapshot[model.MachineDeployment]](s, model.KeyMgmtMachineDeployments)
	if (!ok || kcps.Err != nil) && (!mdOK || mds.Err != nil) {
		return Cell{}, false
	}

	var desired, upToDate int32
	for _, k := range kcps.Items {
		desired += k.DesiredReplicas
		upToDate += k.UpToDateReplicas
	}
	for _, d := range mds.Items {
		desired += d.DesiredReplicas
		upToDate += d.UpToDateReplicas
	}

	cell := Cell{Name: "CAPI"}
	switch {
	case desired == 0:
		cell.Status = StatusLoading
	case upToDate >= desired:
		cell.Status = StatusOK
	default:
		cell.Status = StatusWarn
		cell.Detail = fmt.Sprintf("%d/%d", upToDate, desired)
	}
	return cell, true
}

// cellMachines reports Machines not in a running phase.
func cellMachines(s *store.Store, roles profile.NodeRoles) (Cell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Machine]](s, model.KeyMgmtMachines)
	if !ok || snap.Err != nil {
		return Cell{}, false
	}

	cell := Cell{Name: "Machines"}
	if len(snap.Items) == 0 {
		cell.Status = StatusLoading
		return cell, true
	}
	notRunning := 0
	for _, mc := range snap.Items {
		if mc.Phase != "Running" {
			notRunning++
		}
	}
	switch notRunning {
	case 0:
		cell.Status = StatusOK
	default:
		// Machines transition through non-running phases during any normal
		// rollout, so this is amber rather than red.
		cell.Status = StatusWarn
		cell.Detail = fmt.Sprintf("%d/%d moving", notRunning, len(snap.Items))
	}
	return cell, true
}

// cellHosts reports BareMetalHost errors, the failure a rollout most often
// stalls on.
func cellHosts(s *store.Store, roles profile.NodeRoles) (Cell, bool) {
	snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](s, model.KeyMgmtBareMetalHosts)
	if !ok || snap.Err != nil {
		return Cell{}, false
	}
	return HostsCell(snap.Items)
}

// CellNameHosts is the [Cell.Name] the hosts cell carries, so a consumer
// replacing it in a slice of cells does not have to spell it.
const CellNameHosts = "Hosts"

// HostsCell is the hosts verdict over an explicit set of hosts.
//
// Separate from the store because the BareMetalHost snapshot is deliberately
// *datacenter-wide* — see the watcher, where narrowing it would break the
// unclaimed-host join — while a consumer showing one cluster's hosts has
// narrowed them. The dashboard watches one cluster and shows the whole
// datacenter's hosts, so its cell and its pane agree; a fleet view that scopes
// the pane to the cluster that owns the hardware must scope this too, or the
// cell reports another cluster's failed host with no row on the page to explain
// it.
//
// The verdict itself stays here so both callers make it the same way.
func HostsCell(hosts []model.BareMetalHost) (Cell, bool) {
	cell := Cell{Name: CellNameHosts}
	if len(hosts) == 0 {
		cell.Status = StatusLoading
		return cell, true
	}
	errored := 0
	for _, b := range hosts {
		if b.ErrorMessage != "" || b.OperationalStatus == "error" {
			errored++
		}
	}
	switch errored {
	case 0:
		cell.Status = StatusOK
	default:
		cell.Status = StatusErr
		cell.Detail = fmt.Sprintf("%d errored", errored)
	}
	return cell, true
}

func cellNodes(s *store.Store, roles profile.NodeRoles) (Cell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok || snap.Err != nil {
		return Cell{}, false
	}

	cell := Cell{Name: "Nodes"}
	if len(snap.Items) == 0 {
		cell.Status = StatusLoading
		return cell, true
	}
	notReady, cordoned := 0, 0
	for _, n := range snap.Items {
		if !n.Ready() {
			notReady++
		}
		// A cordon the profile declares expected for this role is the steady
		// state, not a drain, and must not hold the banner at amber for the life
		// of the cluster.
		if n.Cordoned && roles.CordonIsNews(n.Role, n.Ready()) {
			cordoned++
		}
	}
	switch {
	case notReady > 0:
		cell.Status = StatusErr
		cell.Detail = fmt.Sprintf("%d NotReady", notReady)
	case cordoned > 0:
		// Cordoned is expected mid-drain, so it is a warning rather than a
		// failure.
		cell.Status = StatusWarn
		cell.Detail = fmt.Sprintf("%d cordoned", cordoned)
	default:
		cell.Status = StatusOK
	}
	return cell, true
}

func cellPods(s *store.Store, roles profile.NodeRoles) (Cell, bool) {
	snap, ok := store.Get[model.Snapshot[model.Pod]](s, model.KeyWorkloadPods)
	if !ok || snap.Err != nil {
		return Cell{}, false
	}

	cell := Cell{Name: "Pods"}
	if len(snap.Items) == 0 {
		cell.Status = StatusLoading
		return cell, true
	}
	unhealthy := 0
	for _, p := range snap.Items {
		if NeedsAttention(p) {
			unhealthy++
		}
	}
	switch unhealthy {
	case 0:
		cell.Status = StatusOK
	default:
		cell.Status = StatusWarn
		cell.Detail = fmt.Sprintf("%d unhealthy", unhealthy)
	}
	return cell, true
}
