# Point it at a cluster

With no arguments, sextant watches your kubeconfig's current context as both the
management and the workload cluster. That is correct for a self-hosted or
single-cluster setup, and for those you are done.

## See what it would choose

```sh
sextant --list-contexts
```

Every context, which one is current, and which sextant would select for each role.
Reach for this first when it picks the wrong cluster — it shows what the resolver
sees rather than a summary of it.

## Two clusters

Where Cluster API runs somewhere other than the cluster it provisions:

```sh
sextant --management-context capi-management --workload-context prod-workload
```

The management cluster supplies Cluster API, Metal3 and BareMetalHost objects. The
workload cluster supplies nodes, pods, events and every plugin's subsystem — a CNI,
a load balancer and a storage layer live where the workloads do, not where the
controllers do.

The header always names both, because acting on the wrong cluster during a
maintenance window is the mistake this tool exists to prevent.

## Partial names

A context argument need not be an exact name. It may be a substring or a pattern,
which matters when your context names are long and differ in one segment:

```sh
sextant --management-context site-a-mgmt
```

If that matches exactly one context, it is used. If it matches several, sextant asks
which you meant and accepts either a row number or any substring that narrows it to
one. If nothing is attached to answer — in a script, or a pipeline — it errors and
names the candidates instead of guessing.

## Pinning it

Once you know the answer, put it in the config file rather than retyping it:

```yaml
management:
  context: capi-management
workload:
  context: prod-workload
```

See [Configuration](../reference/configuration.md) for precedence. A site whose
context names follow a convention is better served by a
[profile](../reference/profiles.md) with a `context_pattern`, which then works
unchanged across datacentres.

## Rollout mode

Sextant detects a rollout from Cluster API's own replica counts, so it usually needs
no telling. Naming the target version turns rollout mode on *before* the controllers
have replaced anything, which is useful when you are watching the beginning:

```sh
sextant --target-version v1.33.0
```

## Confirming it can read the cluster

```sh
sextant --debug-snapshot -v
```

Starts the watchers, waits for caches to warm, prints one line per data source with
a sample item, and exits. This is the tool for "a pane is empty and I do not know
why" — see [When a pane says nothing](troubleshoot.md).
