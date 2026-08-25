# Your first rollout

By the end of this you will have run the dashboard without a cluster, pointed it at
a real one, and watched a rollout through it. Twenty minutes, and the first half
needs no cluster at all.

## 1. Get it

Download the tarball for your platform from the
[releases page](https://github.com/runlevel-six/sextant/releases) — Linux and macOS,
x86_64 and arm64 — and unpack the binary into the directory you are working in:

```sh
tar -xzf sextant_*_linux_amd64.tar.gz sextant
```

That leaves a `./sextant` binary, which is what the rest of this page assumes. It
needs nothing installed alongside it. To build from source instead, see
[Development](https://github.com/runlevel-six/sextant#development); everything below
works the same either way.

## 2. Look at it before you point it anywhere

```sh
./sextant --demo
```

This is the whole dashboard, driven by invented data. Nothing here touches a cluster
or the network, so it is a safe place to learn the interface.

Take a moment to find these, because they are what you will be reading during a real
upgrade:

- The **header**, naming the version you are running, both clusters and the profile,
  with `ROLLOUT` when one is in progress. A build straight from source reads `dev`
  rather than a version.
- The **banner** below it — one cell per subsystem, quiet when healthy and specific
  when not.
- **Overview**, top left: clusters, rollout progress per pool, node readiness.
- **Machines & Hosts**, which joins Cluster API Machines to the physical hosts
  underneath them. This is the pane that tells you a rollout is stuck on hardware.
  Whatever is moving sorts to the top — provisioning, deprovisioning, a host handed
  back — then anything failed, then the settled fleet.
- **Pod Health**, which shows your critical workloads and every unhealthy pod.

Now try the keys:

- `tab` moves focus. `1`–`9` jump straight to a pane.
- **`z` zooms** the focused pane to the entire screen. Try it on Machines & Hosts —
  on a large cluster this is how you see every row instead of `+ N more`. What
  `+ N more` hides is the quiet part of the fleet, not the machine you are watching.
- `[` and `]` change the column count; `\` returns to automatic.
- `T` cycles themes. `p` freezes the display without stopping the watchers.
- `?` toggles the key hints. `q` quits.

## 3. Point it at your cluster

First, see what it would choose:

```sh
./sextant --list-contexts
```

If the context you want is the current one, just run it:

```sh
./sextant
```

If Cluster API runs on a different cluster from the workloads:

```sh
./sextant --management-context capi-management --workload-context prod-workload
```

If something is empty, do not guess:

```sh
./sextant --debug-snapshot -v
```

One line per data source and per plugin, with a sample item and any error. See
[When a pane says nothing](../how-to/troubleshoot.md).

## 4. Watch a rollout

Sextant detects a rollout from Cluster API's replica counts, so you can simply have
it open when one starts. To put it in rollout mode before the controllers have
replaced anything — useful when you want the target version asserted from the
beginning:

```sh
./sextant --target-version v1.33.0
```

What to watch, roughly in the order things go wrong:

1. **Rollout Progress** in Overview: how many replicas per pool are updated.
2. **Machines & Hosts**: a Machine stuck in `Provisioning` with a host in `error` is
   a rollout waiting on hardware, not on Kubernetes.
3. **Nodes**: one cordoned node is a drain in progress. One `NotReady` that stays
   that way is a node that did not come back.
4. **Pod Health**: what the drain displaced, and what has not been rescheduled.
5. **Events**: the controllers' own account, which is where a stalled drain explains
   itself.
6. The **banner**, throughout — it goes specific the moment a subsystem degrades, so
   you do not have to be looking at the right pane.

## 5. Make it yours

Two things are worth doing once and keeping:

```sh
./sextant --init      # a commented config file to edit
```

Put your contexts in it so you stop typing them. Then, if your cluster has its own
naming conventions — role labels, cordoned-by-design pools, namespaces that matter —
write a [site profile](../how-to/write-a-site-profile.md). That is what stops the
dashboard reporting a permanently cordoned pool as a permanent problem.

## Where to go next

- [Point it at a cluster](../how-to/point-at-a-cluster.md) — contexts and patterns in full.
- [What sextant reports](../explanation/what-it-reports.md) — read this before you
  trust it during a maintenance window.
- [Command line](../reference/cli.md) — every flag and key.
