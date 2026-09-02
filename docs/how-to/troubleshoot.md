# When a pane says nothing

Start here:

```sh
sextant --debug-snapshot -v
```

It starts the watchers, waits for the caches, then prints one line per data source
and one per plugin — whether each detected, and the error if it did not. Most
questions on this page are answered by that output.

## A pane says "detail unavailable"

Ceph, Cilium and OVN read their headline from Kubernetes objects and their *detail*
by running a status command inside a pod. Without `pods/exec` on that namespace they
report the headline and say so, rather than failing.

The message names the reason. Two are worth telling apart:

- **`no pods/exec permission on <namespace>`** — the exec was refused. Usually RBAC.
  It can also be a cluster-wide problem: if the API server's own client certificate
  for talking to kubelets is reissued without the group binding that authorizes
  `nodes/proxy`, every exec, `kubectl logs` and `port-forward` in the cluster fails
  at once. Check whether `kubectl exec` works at all before reaching for RBAC on one
  namespace.
- **`no pod answered (tried N)`** — pods were found and none replied. Expected
  briefly during a rollout, while the pods on a draining node are still listed but
  no longer reachable.

Neither is remembered. Every poll picks a pod again and retries, so a pane recovers
on its own within one interval once anything is reachable — including after you fix
the permission. You should not have to restart it.

## A pane is missing entirely

Plugin panes only exist where their subsystem does, which is deliberate: a cluster
without Ceph should not show an empty Ceph pane. `--debug-snapshot` lists every
plugin and whether it detected.

If a plugin you *do* run is absent, the usual cause is that its probe failed at
startup. Detection runs once, when the dashboard starts. A plugin that answers
"absent" is gone for the session; one that answers "present but unreachable" keeps
its pane and retries. If a subsystem was genuinely down at launch and has since
recovered, restart sextant.

`--debug-snapshot` reports the error either way, which is the difference between "we
do not run that" and "it did not answer".

## A table says "+ N more"

The pane has more rows than the terminal gave it. Press `z` to zoom the focused pane
to the whole screen — that is what zoom is for, and on a large fleet it is the
difference between 25 machines and all of them. A zoomed table wide enough to fit
twice over flows its rows into columns rather than hiding them.

`[` and `]` change the column count, which trades width for height. `\` returns to
automatic.

Some truncation is arithmetic rather than a bug: three tables that each want fifty
rows cannot all have them on a seventy-row terminal.

## Startup is slow

Detection happens before the first frame, and every probe is a network round trip
that can time out rather than fail. Two commands separate the causes:

```sh
time sextant --dry-run        # config and kubeconfig only
time sextant --debug-snapshot # adds watchers, detection and one poll per source
```

If `--dry-run` is fast and `--debug-snapshot` is slow, it is a probe waiting on
something unreachable. Probes run concurrently and each is bounded, so the cost is
the slowest single one rather than their sum — but an unreachable API can still
account for several seconds.

## Nodes are cordoned and the banner is amber forever

Some pools are cordoned by design, with capacity reserved for something the
scheduler must not use. Tell the profile which roles those are:

```yaml
node_roles:
  cordon_expected: [compute]
```

Without it, a permanently cordoned pool reads as mid-drain. See
[Site profiles](../reference/profiles.md).

## Machines from clusters you did not ask about

One management cluster can own several workload clusters, and by default every
Cluster API object it holds is read. That puts three clusters' Machines beside one
cluster's Nodes, and the rollout detector — which reads those Machines — then answers
a different question from the one you asked.

Narrow it in the profile with `capi_name` or `capi_name_pattern`.

## The screen looks wrong on a light terminal

Try `--theme ncurses` or `--theme lcars`. Both paint their own background, so they
render identically regardless of your terminal's colors. `--theme ansi` does the
opposite and inherits your scheme entirely.
