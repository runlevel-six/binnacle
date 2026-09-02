# Command line

Every flag, environment variable and key binding. `sextant --help` prints the same
flag list; this page adds what each one is for.

Run with no arguments and sextant watches your kubeconfig's current context as both
the management and the workload cluster, which is the single-cluster case.

## Selecting clusters

| Flag | Default | Purpose |
|---|---|---|
| `--management-context` | current context | Where Cluster API, Metal3 and BareMetalHost objects live. |
| `--workload-context` | same as management | Where nodes, pods and events live. Set it when the two are different clusters. |
| `--kubeconfig` | `$KUBECONFIG` | Read one file instead of the usual search path. |

A context may be given as a substring or pattern rather than an exact name; if it
matches more than one context sextant asks which you meant, and errors naming the
candidates when nothing is attached to answer. See
[Point it at a cluster](../how-to/point-at-a-cluster.md).

## Behaviour

| Flag | Default | Purpose |
|---|---|---|
| `--profile` | built-in `metal3` | Site profile to apply, by name or path to a YAML file. |
| `--target-version` | — | The version you are rolling to. Setting it turns on rollout mode before the controllers have replaced anything. |
| `--os-cloud` | `$OS_CLOUD`, then the profile | Which `clouds.yaml` entry the OpenStack plugin should use. |
| `--theme` | `default` | Color scheme. See [Themes](themes.md). |
| `--config` | `~/.config/sextant/config.yaml` | Config file path. |

## Looking rather than watching

| Flag | Purpose |
|---|---|
| `--demo` | Run against invented data. No kubeconfig, no cluster, no network. |
| `--demo-fleet` | Run the fleet screen against invented fleet data. No server, no kubeconfig, no network. |
| `--render WxH` | With `--demo` or `--demo-fleet`, print one frame at that size and exit. No TTY needed. |
| `--dry-run` | Resolve configuration, print what would be watched, and stop. |
| `--debug-snapshot` | Start the watchers, summarize every data source, and exit. The first thing to reach for when a pane is empty. |
| `--debug-duration` | How long `--debug-snapshot` waits for caches to warm. Default 10s. |
| `-v` | With `--debug-snapshot`, also print a sample item per source. |
| `--list-contexts` | Every kubeconfig context, and which one sextant would select. |
| `--list-profiles` | Every profile that can be loaded, and where it looked. |
| `--list-themes` | Every color scheme. |
| `--init` | Write an example config file and exit. |
| `--version` | Print version, commit, build date and toolchain. The version alone is also shown in the dashboard header, beside the name. |

## Fleet mode

Instead of reading a kubeconfig, sextant can connect to a binnacle server and
show a fleet of clusters. The server provides the JSON API and SSE stream;
sextant renders the fleet list and per-cluster detail in the terminal.

| Flag | Purpose |
|---|---|
| `--server URL` | Connect to a binnacle server at this URL instead of reading a kubeconfig. |
| `--server-cluster NS/NAME` | With `--server`, skip the fleet list and go straight to one cluster's detail. Press Esc to return to the fleet. |
| `--token` | Bearer token for `--server`. A server running with `--allow-unauthenticated` does not need one. |

These can also be set in the config file's `server:` section or via environment
variables; see [Configuration](configuration.md).

## Environment variables

Each corresponds to a flag. The flag wins; see
[Configuration](configuration.md) for the full precedence.

| Variable | Equivalent flag |
|---|---|
| `SEXTANT_MANAGEMENT_CONTEXT` | `--management-context` |
| `SEXTANT_WORKLOAD_CONTEXT` | `--workload-context` |
| `SEXTANT_PROFILE` | `--profile` |
| `SEXTANT_TARGET_VERSION` | `--target-version` |
| `SEXTANT_THEME` | `--theme` |
| `SEXTANT_SERVER` | `--server` |
| `SEXTANT_SERVER_CLUSTER` | `--server-cluster` |
| `SEXTANT_SERVER_TOKEN` | `--token` |
| `OS_CLOUD` | `--os-cloud` |

`OS_CLOUD` is deliberately not `SEXTANT_`-prefixed. It is the OpenStack
ecosystem's own variable, read by the `openstack` CLI and every SDK; an operator
switching between clouds has already exported it.

## Keys

### Single-cluster dashboard

| Key | Action |
|---|---|
| `?` | Toggle the key hints in the footer. |
| `q` | Quit. |
| `tab` / `shift+tab` | Cycle focus forward and back, including panes the layout had to hide. |
| `1`–`9` | Jump straight to a pane. The digit is shown in its title and does not move when you resize. |
| `z` | Zoom: give the focused pane the entire screen. Press again to return. |
| `[` / `]` | Fewer or more columns than the width would choose. |
| `\` | Back to automatic column count. |
| `p` | Freeze the display. Watchers keep running; the screen stops changing. |
| `T` | Cycle themes. |

### Fleet screen (`--server` or `--demo-fleet`)

| Key | Action |
|---|---|
| `?` | Toggle the key hints in the footer. |
| `↑` / `↓` or `k` / `j` | Move selection up or down. |
| `enter` | Drill into the selected cluster. In `--demo-fleet`, shows the full dashboard from the demo fixture. In `--server` mode, opens a per-cluster SSE stream and shows the full dashboard from live data. |
| `esc` | Return to the fleet list. |
| `r` | Reverse sort order (worst-first ↔ worst-last). |
| `/` | Filter clusters by substring (case-insensitive). Type to narrow, enter to apply, esc to clear. |
| `q` | Quit. |

`z` is the answer to "this table is truncating". On a large cluster a pane may show
`+ 38 more`; zooming gives it every row the terminal has, and its full width — so any
cell the grid had to cut is whole again. If the rows still outrun the screen and the
table is narrow enough for the pane to hold it twice, they flow into side-by-side
columns, filling downwards and then across.
