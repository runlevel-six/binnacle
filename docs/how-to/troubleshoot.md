# When a pane says nothing

Start here:

```sh
sextant --debug-snapshot -v
```

It starts the watchers, waits for the caches, then prints one line per data source
and one per plugin — whether each detected, and the error if it did not. Most
questions on this page are answered by that output.

**Most of this page applies to both front ends.** A thin pane, an absent
subsystem, a permanently amber banner and a truncated table are properties of
the data and the verdicts, which `binnacle` and `sextant` share — so the same
explanation and the same fix hold whether you are looking at a terminal or a web
page, and `--debug-snapshot` is worth running against the same cluster either
way. Two sections are terminal-only and say so. [The server has its own
symptoms](#the-server-specifically) at the end.

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

*Terminal only.* Try `--theme ncurses` or `--theme lcars`. Both paint their own
background, so they render identically regardless of your terminal's colors.
`--theme ansi` does the opposite and inherits your scheme entirely.

## The server specifically

Symptoms only `binnacle` has. The [deployment guide](../../deploy/README.md) is
the reference for the settings named here.

**The page loads and then stops updating.** Every page is pushed over
Server-Sent Events, so the connection is long-lived by design and a proxy read
timeout shorter than the stream will cut it. The failure is quiet: the page keeps
rendering whatever it last received and simply stops changing. The `live`
indicator in the header goes to "reconnecting…" when the browser notices, so
check that first. Binnacle sends a keepalive every 25 seconds; the ingress
annotations it ships cover nginx and Traefik.

**Signing in loops back to the sign-in page.** With more than one replica,
sessions signed by one pod are rejected by the others. Set
`$BINNACLE_SESSION_KEY` explicitly rather than letting each pod generate its
own — otherwise every request that lands on a different pod is a fresh session.

**The page serves an old build.** If the image reference is a moving tag —
`latest` or `edge` — and `imagePullPolicy` is left at the default
`IfNotPresent`, a node that has already cached that tag keeps running the image
it has. The Deployment reports Available, `rollout restart` does nothing, and
nothing anywhere says the version is stale. Pin a release, ideally with its
digest: `binnacle:1.8.0@sha256:…`. `binnacle --version`, or the version in the
page footer, tells you what is actually running.

**A script or a client gets HTML instead of JSON.** It is not sending a
credential. Every `/api/v1/` route except `authinfo` needs a session cookie or
an `Authorization: Bearer` ID token, and answers **401** without one — so an
HTML body means something followed a redirect it should not have, or the request
went somewhere other than the API. See
[binnacle server](../reference/binnacle-server.md).

**The management section shows nothing, or counts it cannot explain.** It reads
the management cluster's own nodes and pods, which needs a cluster-scoped read
that a namespaced Role cannot grant — nodes are not namespaced. If the section
renders two loading cells forever, that grant is missing. For which pods are
behind a count, follow the panel's title to [`/management`](../reference/binnacle-server.md#pages).
