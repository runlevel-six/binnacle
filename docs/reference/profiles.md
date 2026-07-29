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

critical_workloads:            # named workloads always shown in Pod Health
  - namespace: ingress
    kind: Deployment
    name: ingress-nginx

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
