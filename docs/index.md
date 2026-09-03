# Documentation

Monitoring for rolling Kubernetes upgrades on bare metal — Cluster API, Metal3,
and the subsystems that break when a host goes away. Two front ends onto one
data layer: **binnacle** serves the whole fleet as a web page, **sextant** is
the terminal client for one cluster at a time. Start with the
[README](../README.md) for what this is and how to install it; the pages below
go deeper.

If you are about to watch a real maintenance window, read
**[What it reports](explanation/what-it-reports.md)** first. It sets out what
these tools claim, what they refuse to claim, and where they will tell you they
do not know — which is the part that matters at 3am. It applies to both front
ends, because the judgements are shared code rather than two implementations.

## Tutorials — learning by doing

- [Your first rollout](tutorials/first-rollout.md) — the demo dashboard, then a real cluster, then a rollout.

## How-to guides — a specific goal

- [Point it at a cluster](how-to/point-at-a-cluster.md) — contexts, separate management and workload clusters, ambiguity.
- [Write a site profile](how-to/write-a-site-profile.md) — teach it your naming, roles and plugin layout.
- [Run without a cluster](how-to/run-without-a-cluster.md) — demo mode, and regenerating the screenshots.
- [Connect to a binnacle server](how-to/connect-to-a-server.md) — the whole fleet, with no cluster credentials of your own.
- [Deploy the server](../deploy/README.md) — manifests, RBAC, OIDC, per-user scoping, and what to pin.
- [When a pane says nothing](how-to/troubleshoot.md) — missing detail, absent panes, slow startup.

## Reference — look it up

- [Command line](reference/cli.md) — sextant's flags, environment variables and key bindings.
- [binnacle server](reference/binnacle-server.md) — the server's flags, its pages, and the read API.
- [Configuration](reference/configuration.md) — the config file, and precedence between flag, env and file.
- [Site profiles](reference/profiles.md) — every profile key, and the built-in profiles.
- [Themes](reference/themes.md) — the four themes and what a theme may change.

## Explanation — why it works this way

- [What it reports](explanation/what-it-reports.md) — design intent: tiers, absence versus unreachability, and the rules about what may be shown as fact.
- [Architecture](explanation/architecture.md) — one data layer under two front ends: the store, panes as pure functions, the core/plugin boundary, layout.

## Something's wrong

- [When a pane says nothing](how-to/troubleshoot.md) — start here.
