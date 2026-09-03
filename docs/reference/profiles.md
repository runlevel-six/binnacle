# Site profiles

A profile describes how a cluster is *laid out* — what its node-role labels are
called, which namespaces matter, how its panes should be arranged. It says nothing
about which cluster you are watching or how you like it to look; those are
[configuration](configuration.md).

That split is the point. A profile is meant to be shared across a team and across
datacentres, so it must not contain anything that changes per session.

## Built-in profiles

| Name | For |
|---|---|
| `metal3` | Vanilla Cluster API + Metal3. The default, used when you name nothing. |
| `openstack` | OpenStack on Kubernetes, provisioned by Cluster API and Metal3. |

`sextant --list-profiles` lists these plus anything found in
`~/.config/sextant/profiles` and `./profiles`. `--profile` also accepts a path to a
YAML file, which is the easy way to iterate on one.

## Keys

```yaml
name: my-site
description: What this profile is for
extends: metal3          # inherit, then override the keys below

clusters:
  management:
    context: capi-management     # exact kubeconfig context
    context_pattern: '^capi-'    # or a regex over name, cluster and user
    namespaces: [capi-system]    # empty reads all, which is upstream's default
  workload:
    context_pattern: 'site-a-tenant'
    # Which Cluster API Cluster object corresponds to this cluster. Set it where
    # one management cluster owns several and only one is being watched.
    capi_name: tenant-01-cluster
    # Or derive it from the resolved context name. Preferred over a literal,
    # because a literal makes the profile single-cluster.
    capi_name_pattern: '([a-z]+-\d+)$'

node_roles:
  # Consulted in order; first key that yields a role wins. A key ending in /* is
  # a prefix match whose suffix is the role. Put a platform-specific key first so
  # it beats the upstream fallback.
  label_keys:
    - my-platform/role
    - node-role.kubernetes.io/*
  display:                     # raw role -> label shown in a pane
    controller: Control-Plane
  machinedeployment_match:     # role -> substrings identifying it in an MD name
    compute: [compute, worker]
  # Roles whose nodes are cordoned as a steady state, not mid-drain. Without
  # this, a permanently cordoned pool holds the node banner at amber forever.
  cordon_expected: [compute]

events:
  namespaces: [capi-system]    # which namespaces the workload event watch reads
  all_namespaces: false        # true widens it, at a cost on a busy cluster

critical_workloads:            # workloads pinned on the cluster being watched
  - namespace: ingress
    kind: Deployment
    name: ingress-nginx

management_workloads:          # workloads pinned on the *management* cluster
  - namespace: capi-system
    kind: Deployment
    name: capi-controller-manager

plugins:                       # per-plugin settings; keys vary by plugin
  ceph:
    namespace: rook-ceph
  cilium:
    namespace: kube-system

layout:                        # optional pane arrangement overrides
  top_row: [overview]
  grid: [machines, nodes, pods, events]
  stack:
    - under: pods
      kind: events
      ratio: 0.4
```

## Two lists, two clusters

**`critical_workloads`** pins workloads on the cluster being watched — the
workload cluster, whose components you want to see whether or not they are
currently failing. They render in the terminal's Pod Health pane and in the web
UI's Critical Workloads section on that cluster's page.

**`management_workloads`** pins workloads on the *management* cluster: the
controllers whose failure stops every workload cluster reconciling. They render
in the management panel and page.

Keep them separate even when it feels like duplication, because reusing one for
both is wrong in two directions at once. Checked against the wrong cluster, a
workload cluster's database reports **absent** — true, and meaningless. And
where a name exists on both clusters, as an ingress controller or a monitoring
agent easily does, it reports **healthy**: a green verdict about an object
nobody asked about, which is the harder failure to notice.

Neither list has a default. A profile that declares no management workloads
renders no controller table, which is the right answer — no table beats a table
of wrong rows. On a cluster built by `clusterctl` the conventional names are:

```yaml
management_workloads:
  - {namespace: capi-system, kind: Deployment, name: capi-controller-manager}
  - {namespace: capi-kubeadm-bootstrap-system, kind: Deployment, name: capi-kubeadm-bootstrap-controller-manager}
  - {namespace: capi-kubeadm-control-plane-system, kind: Deployment, name: capi-kubeadm-control-plane-controller-manager}
  - {namespace: baremetal-operator-system, kind: Deployment, name: baremetal-operator-controller-manager}
```

Confirm each one exists before pinning it: an entry that matches nothing renders
a red **absent** row, so a stale name is a false alarm rather than a harmless
no-op. Matching is on namespace plus a `name-` pod prefix, which covers both a
StatefulSet's `name-0` and a Deployment's `name-<replicaset>-<pod>`.

## Two keys worth dwelling on

**`cordon_expected`** exists because some pools are cordoned by design — capacity
reserved for something the scheduler must not use. Without naming those roles, every
such node reads as mid-drain and the node banner sits amber for the life of the
cluster. Ask what a cluster's *steady state* is, not just what its schema is.

**`capi_name_pattern`** narrows Cluster API objects to one cluster. A management
cluster owning three clusters otherwise shows three clusters' Machines beside one
cluster's Nodes — and the rollout detector, which reads those Machines, is then
answering a different question from the one you asked. Filtering at the source makes
the whole dashboard agree.

## Checking a profile

```sh
sextant --profile ./my-site.yaml --dry-run
```

Validation problems are reported by name, and an unknown profile is an error rather
than a silent fallback: quietly ignoring a requested profile would produce a
dashboard that starts, looks right, and reports the wrong things.
