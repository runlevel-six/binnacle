# What sextant reports

This page is design intent rather than instructions. It sets out what this tool
claims, what it refuses to claim, and how it says "I do not know" — which is the
part that matters when you are reading it during a maintenance window at 3am.

If you take one thing from it: **a dashboard's job is not to look confident.** Every
rule below exists because a plausible-looking wrong answer cost somebody real time.

## The principles

**Never report a number the source is not entitled to report.** This is the most
expensive lesson in the project. A distributed database exposes, on every member, a
"last message from peer" age. Reading it from any member looks like a health signal.
It is not: in a Raft cluster the leader heartbeats its followers and they answer the
leader, so two *followers* have no reason to exchange anything between elections. A
follower's figure for another follower therefore grows without bound on a perfectly
healthy cluster — measured on a live one, a follower reported its peer as 4.9 hours
silent while the leader had heard from both members 46ms earlier and had them fully
replicated. The pane now asks the leader and reports staleness only from there.

The general form: a real payload, correctly parsed, can still be wrongly
*interpreted*. Ask who is entitled to answer a question, not only whether the field
parses.

**Distinguish "absent" from "I could not reach it".** These are different answers and
only the subsystem can tell them apart. "This cluster does not run Ceph" is
permanent and means no pane at all. "I could not reach Ceph just now" is a fact about
one second. Detection runs once, at startup, so treating the second as the first
deletes a pane for the whole session over something that recovered immediately —
and the moment most likely to fail a probe is a control-plane rollout, which is
exactly when you want the pane.

**A detector may record verdicts, never symptoms.** Anything that can change without
you doing something — a pod's readiness, a node's health, a cluster member's role —
must be re-derived when it is read, not decided at startup. This is the same rule as
above, one level up, and it is why no pane stays degraded after the cluster recovers.

**Say why a pane is thin.** Where detail needs a command run inside a pod, the
absence of that permission is reported as itself: the pane shows what the
Kubernetes objects alone say and names the reason it can show no more. An operator
with tighter RBAC than the author's should get a thinner pane, not an error.

**A theme colors chrome and never rewrites data.** Labels and titles may be shouted;
a context name, a cluster name, a pod name never is. `Prod-Tenant-01` is not the
name of anything, and an interface that renames things is lying about them.

**Absence of alarm is not proof of health.** A pane with no data says so — "waiting
for data", or the error that stopped it — rather than rendering an empty table that
looks like good news.

**Prefer a truthful gap to a confident guess.** An unrecognized status renders as "no
opinion" rather than being sorted into healthy or broken. A capacity figure with no
denominator renders as `—`. Where the tool cannot tell, it says so.

## Three tiers of detail

Every plugin reports at one of three levels, and says which:

- **Absent** — the subsystem is not here. No pane, no banner cell, nothing.
- **Informer** — present, and readable only through Kubernetes objects. A version,
  a replica count, a health enum. This is what you get without `pods/exec`, and it
  is a legitimate operating mode rather than a failure.
- **Full** — a status command ran inside a pod and its output was parsed.

A pane at the informer tier names the reason. The tier is re-derived on every poll,
so it climbs back to full on its own.

## What it deliberately does not do

**It does not write.** Sextant reads. It has no verbs — no drain, no cordon, no
retry, no approve. A dashboard you can act from is a dashboard that can act by
accident during an upgrade, and the blast radius of a misread pane should be your
attention, not your cluster.

**It does not alert.** No thresholds you configure, no notifications, no history. It
shows the present moment to somebody who is already looking. Ranking severity is
your monitoring system's job and it is better at it.

**It does not model your organization.** Site conventions live in a
[profile](../reference/profiles.md) as data. There is no hardcoded namespace, no
assumed label, no built-in idea of what a compute node is called — every one of
those assumptions was wrong on the first real cluster it met.

**It does not pretend one cluster is another.** Where the management and workload
clusters differ, both are named in the header, always.

## Why bare metal changes the questions

On a cloud provider, a node that will not come back is replaced and forgotten. On
bare metal it is a machine in a rack with a BMC that is not answering, and the
rollout is stopped until somebody deals with it.

That is why Machines and hosts share a pane: a Cluster API Machine stuck in
`Provisioning` means nothing on its own, and means "this host failed introspection"
when joined to the BareMetalHost underneath it. It is also why cordoned nodes are
first-class rather than an error state — on bare metal a permanently cordoned pool
is a normal way to reserve capacity, and a dashboard that reads it as a problem is
a dashboard nobody trusts by the third day.
