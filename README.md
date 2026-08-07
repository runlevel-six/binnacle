<p align="center">
  <img src="docs/logo.png" alt="sextant" width="320">
</p>

<p align="center">
  <a href="https://github.com/runlevel-six/sextant/actions/workflows/ci.yaml"><img src="https://github.com/runlevel-six/sextant/actions/workflows/ci.yaml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/runlevel-six/sextant"><img src="https://pkg.go.dev/badge/github.com/runlevel-six/sextant.svg" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License"></a>
</p>

A terminal dashboard for **Cluster API on bare metal**.

Watch a rolling upgrade move across `Machine` → `Metal3Machine` →
`BareMetalHost`, and see the workload cluster's reaction to it — nodes cordoning,
pods going unready, events firing — on a single screen. Built to replace the
tmux-of-`watch`-commands that lives next to a maintenance-window runbook.

> The dashboard runs. Core panes — rollout overview, machines joined to hosts,
> nodes, pod health, events — are in place, profiles load from YAML, and the
> optional subsystem panes (Ceph, Cilium, MetalLB, OVN, OpenStack) appear when
> those subsystems are detected.

## Install

Releases attach statically linked binaries for Linux and macOS on x86_64 and
arm64. No runtime dependencies, and no Go toolchain needed unless you want one.

**A tagged release** — the one to pin. Grab the tarball for your platform from the
[releases page](https://github.com/runlevel-six/sextant/releases), which prints the
exact `curl` line for each, then:

```sh
tar -xzf sextant_*_linux_amd64.tar.gz sextant
install -m 0755 sextant ~/.local/bin/sextant
```

**Current `main`** — the `edge` prerelease is rebuilt on every merge, so this URL
always resolves to the tip of the branch. Substitute `linux_arm64`, `darwin_amd64`
or `darwin_arm64` as needed:

```sh
curl -sSfL https://github.com/runlevel-six/sextant/releases/download/edge/sextant_edge_linux_amd64.tar.gz \
  | tar -xz sextant
install -m 0755 sextant ~/.local/bin/sextant
```

`edge` reports its version as `edge`, carries no release notes, and is replaced
without warning. It is for trying out unreleased work, not for depending on.

**With Go:**

```sh
go install github.com/runlevel-six/sextant/cmd/sextant@latest
```

`@latest` resolves to the newest tagged release. Note that a `go install` build
reports its version as `dev`: the ldflags that stamp version, commit and date are
applied by the Makefile and the release pipeline, not by `go install`.

Every release ships a `checksums.txt`; verify with
`sha256sum --ignore-missing -c checksums.txt`.

## Try it

Examples below are written `./sextant`, for a binary in the current directory —
drop the `./` if you installed it onto your `PATH`.

```sh
./sextant --demo              # the whole dashboard, no cluster needed
./sextant --list-contexts     # what the resolver sees, and what it picked
./sextant --debug-snapshot -v # can it read your cluster? one line per source
./sextant                     # the dashboard, on your current context
```

`--demo` runs against invented data — a control-plane rollout mid-flight, a host
that failed to provision, a node that has not come back — so you can see what the
tool does before pointing it at anything. Every screenshot below is a `--demo`
frame, which is also how they are regenerated:

```sh
./sextant --demo --render 280x84   # one frame to stdout, no TTY required
```

Keys: `?` help, `tab` cycle focus, `1`–`9` jump, `z` zoom, `[`/`]` columns,
`p` freeze, `T` theme, `q` quit.

## Themes

```sh
./sextant --list-themes
./sextant --theme lcars
```

![sextant in the default theme](docs/theme-default.png)

| Theme | Look | |
|---|---|---|
| `default` | green/amber/red on rounded borders | [screenshot](docs/theme-default.png) |
| `ansi` | the terminal's own sixteen colors, so it inherits your scheme | [screenshot](docs/theme-ansi.png) |
| `lcars` | LCARS-style console: black ground, block rails, amber and violet | [screenshot](docs/theme-lcars.png) |
| `ncurses` | DOS-era curses: blue panels, double-line boxes, white ink | [screenshot](docs/theme-ncurses.png) |

Set one with `--theme`, `SEXTANT_THEME`, or `theme:` in the config file, or press
`T` to cycle through them live. A theme colors the chrome and the status
palette; it never rewrites data, so a context or cluster name reads the same
under all of them.

`lcars` is what it sounds like. It is a real theme rather than a hidden flag —
`--list-themes` names it, and the health colors still mean what they mean — but
nobody will mistake it for the sober option.

It is an homage built from color values and box-drawing characters: no fonts,
artwork, or images from any source are included. This project is not affiliated
with, endorsed by, or sponsored by CBS Studios or Paramount, and Star Trek and
LCARS are the trademarks of their respective owners.

`lcars` and `ncurses` paint their own background rather than letting the
terminal's show through — every cell of the screen, header and footer included —
so they look the same on a light terminal as on a dark one. For `lcars` that is
not a preference: an LCARS panel is a colored block on an unlit screen, and the
black between the rails is the display itself, so the look does not survive being
dropped on a pale terminal.

`ncurses` is for anyone who spent the nineties in `dialog` and `menuconfig`.

## Documentation

Full documentation is in [docs/](docs/index.md), organized by what you are trying to
do — [a first rollout](docs/tutorials/first-rollout.md) to learn it, how-to guides
for a specific goal, reference for looking things up, and explanation for why it
works the way it does.

Before you rely on it during a maintenance window, read
**[What sextant reports](docs/explanation/what-it-reports.md)** — what this tool
claims, what it refuses to claim, and how it says "I do not know".

## Why

`clusterctl describe` gives you a static tree. k9s browses resources but sees
CAPI objects as opaque CRs — it can't tell you that a `KubeadmControlPlane` is
3/5 rolled, or join a `Machine` to the physical host underneath it. Web
dashboards want a browser and a port-forward, which is the wrong shape when
you're SSH'd into a jump host at 2am during a maintenance window.

sextant is for that window.

## Design goals

- **Zero-config on a stock cluster.** Point it at a CAPI + Metal3 management
  cluster and it works. No site-specific setup required to see something useful.
- **Site-specific behavior lives in data, not code.** Node-role label keys,
  interesting namespaces, critical workloads and pane layout come from a YAML
  *profile*. Core Go contains no site-specific string literals.
- **Optional subsystems auto-detect.** Ceph, Cilium, MetalLB, OVN and OpenStack
  panes appear when those subsystems are present and disappear when they aren't
  — never an error you have to configure away.
- **Degrade, don't fail.** Missing `pods/exec` means a thinner pane, not a
  stack trace.
- **Read-only.** sextant never issues a mutating API call.

## Development

```sh
git clone https://github.com/runlevel-six/sextant.git
cd sextant
make check    # fmt + vet + test
make build    # ./sextant
make help     # all targets
```

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The most useful
thing you can offer right now is a description of your cluster's shape, since
the default profile is only as good as the range of clusters we know about.

## License

[Apache 2.0](LICENSE)
