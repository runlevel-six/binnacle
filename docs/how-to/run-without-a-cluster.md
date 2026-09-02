# Run without a cluster

```sh
sextant --demo
```

Demo mode renders the whole dashboard from invented data — no kubeconfig, no
cluster, no network. Useful for three things: seeing what the tool does before
pointing it at anything, working on panes and layout without a cluster to hand, and
producing screenshots that are safe to publish.

The fixture is deliberately not a healthy cluster. A screenshot of an idle
dashboard argues nothing, so it shows a control-plane rollout mid-flight, a
BareMetalHost that failed to provision, a node that has not come back, unhealthy
pods, storage rebalancing after losing an OSD, and live migrations in progress.

## One frame, without a terminal

```sh
sextant --demo --render 280x84 > frame.ansi
```

`--render` builds one frame at exactly that size, prints it, and exits. No TTY and
no alternate screen, so it works in a pipe or a CI job. Two runs produce identical
bytes: the fixture holds *durations* rather than timestamps, so an age column reads
the same every time and a regenerated screenshot is not a spurious diff.

## Regenerating the screenshots

The images in `docs/` are demo frames. To refresh them after a layout change, run
each theme at the size you want and capture the terminal:

```sh
for theme in default ansi lcars ncurses; do
  sextant --demo --theme "$theme"    # then screenshot the window
done
```

Or convert the ANSI directly, with any tool that renders escape sequences to an
image:

```sh
sextant --demo --render 280x84 --theme lcars > lcars.ansi
```

## Why the fixture uses the names it does

Addresses come from `192.0.2.0/24` (RFC 5737) and hostnames from the `.example` TLD
(RFC 6761), both reserved for documentation. Nothing in the fixture can collide with
a real deployment, and a test asserts it: the rendered frame is checked for private
address ranges and real-world hostname suffixes, so a leak fails the build rather
than reaching a published image.

That matters because screenshots of a real cluster cannot be published. A single
frame carries hostnames, kubeconfig context names, address pools and a workload
inventory. `/*.png` is gitignored at the repository root for the same reason — the
demo is the supported way to produce an image worth sharing.
