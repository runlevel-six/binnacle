# Documentation

A terminal dashboard for rolling Kubernetes upgrades on bare metal — Cluster API,
Metal3, and the subsystems that break when a host goes away. Start with the
[README](../README.md) for what this is and how to build it; the pages below go
deeper.

If you are about to watch a real maintenance window, read
**[What sextant reports](explanation/what-it-reports.md)** first. It sets out what
this tool claims, what it refuses to claim, and where it will tell you it does not
know — which is the part that matters at 3am.

## Tutorials — learning by doing

- [Your first rollout](tutorials/first-rollout.md) — the demo dashboard, then a real cluster, then a rollout.

## How-to guides — a specific goal

- [Point it at a cluster](how-to/point-at-a-cluster.md) — contexts, separate management and workload clusters, ambiguity.
- [Write a site profile](how-to/write-a-site-profile.md) — teach it your naming, roles and plugin layout.
- [Run without a cluster](how-to/run-without-a-cluster.md) — demo mode, and regenerating the screenshots.
- [When a pane says nothing](how-to/troubleshoot.md) — missing detail, absent panes, slow startup.

## Reference — look it up

- [Command line](reference/cli.md) — every flag, environment variable and key binding.
- [Configuration](reference/configuration.md) — the config file, and precedence between flag, env and file.
- [Site profiles](reference/profiles.md) — every profile key, and the built-in profiles.
- [Themes](reference/themes.md) — the four themes and what a theme may change.

## Explanation — why it works this way

- [What sextant reports](explanation/what-it-reports.md) — design intent: tiers, absence versus unreachability, and the rules about what may be shown as fact.
- [Architecture](explanation/architecture.md) — the store, panes as pure functions, the core/plugin boundary, layout.

## Something's wrong

- [When a pane says nothing](how-to/troubleshoot.md) — start here.
