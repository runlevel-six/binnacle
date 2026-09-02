# Write a site profile

A profile teaches sextant your cluster's conventions: what its role labels are
called, which pools are cordoned on purpose, which namespaces matter. Without one it
uses upstream Cluster API and Metal3 defaults, which is correct for a stock cluster
and wrong for most real ones.

Every key is in the [profile reference](../reference/profiles.md). This page is the
order to do it in.

## 1. Start from a built-in

```sh
sextant --list-profiles
```

Copy nothing — inherit instead:

```yaml
name: my-site
description: My platform, two datacentres
extends: metal3
```

`extends` takes the built-in and overrides only what you name, so your file stays
short and picks up improvements to the base.

Iterate with a path, not a name:

```sh
sextant --profile ./my-site.yaml --dry-run
```

## 2. Fix the node roles first

This is the key that most changes what you see, because roles drive the Overview
pane's grouping and the Machines pane's ROLE column.

```yaml
node_roles:
  label_keys:
    - my-platform/role            # yours first
    - node-role.kubernetes.io/*   # upstream fallback
```

Keys are consulted in order and the first that yields a role wins. A trailing `/*`
is a prefix match whose suffix becomes the role.

**Put your own key first.** If the upstream fallback runs first it may return a
different spelling of the same role than your MachineDeployment names use, and the
overview then splits one pool across two role names that look like two pools.

Then map the raw values to what you call them:

```yaml
  display:
    ctrl: Control-Plane
  machinedeployment_match:
    compute: [compute, worker]
```

## 3. Declare the cordons you mean

```yaml
  cordon_expected: [compute]
```

If a pool is cordoned as its steady state — capacity reserved for something the
scheduler must not use — say so. Otherwise every such node reads as mid-drain and the
node banner sits amber for the life of the cluster.

This is the key people discover late, so it is worth asking directly: **what is
normal on this cluster that would look wrong to a stranger?**

## 4. Narrow to one cluster, if the management cluster owns several

```yaml
clusters:
  workload:
    capi_name_pattern: '([a-z]+-\d+)$'
```

Otherwise all of them are read, and you get three clusters' Machines beside one
cluster's Nodes — with the rollout detector reading those same Machines and therefore
answering a different question from the one you asked.

Prefer a pattern to a literal name. A literal makes the profile single-cluster, and
the same file then cannot serve a second datacentre whose contexts differ in one
segment.

## 5. Contexts, if they follow a convention

```yaml
clusters:
  management:
    context_pattern: '-mgmt-'
  workload:
    context_pattern: '-tenant-'
```

A pattern is matched against each context's name, cluster and user. This is what
makes one profile work across sites: the operator picks the datacentre, the profile
picks the roles within it.

Leave these out on a stock cluster — the current context is the right default.

## 6. Namespaces and critical workloads

```yaml
events:
  namespaces: [capi-system]

critical_workloads:
  - namespace: ingress
    kind: Deployment
    name: ingress-nginx
```

Event watches are the most expensive thing here. Scoping them to the namespaces you
care about matters on a busy management cluster; widening to all of them is a
deliberate choice with a cost.

Critical workloads are pinned to the top of Pod Health whether or not they are
unhealthy, so you can see at a glance that the thing you care about survived a drain.

## 7. Plugin settings, only where discovery is wrong

```yaml
plugins:
  ceph:
    namespace: rook-ceph
```

Try without this first. Plugins discover their own namespaces and workload names,
because every hardcoded name in this project's history was wrong on the first real
cluster it met. Pin one only when discovery gets it wrong — and then it is worth
reporting, since discovery could probably handle your case too.

## 8. Install it

Drop the file in `~/.config/sextant/profiles/` (or `./profiles/`) and it becomes
available by name:

```yaml
profile: my-site
```

in your config file, so you stop passing `--profile`.

**A profile is shared; keep session-specific things out of it.** Which cluster you
are watching, which cloud, which theme — those belong in
[configuration](../reference/configuration.md) or on the command line. A profile that
names one datacentre cannot be used in the other.
