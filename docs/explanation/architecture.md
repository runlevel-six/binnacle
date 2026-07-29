# Architecture

Why the code is shaped the way it is. Useful if you are adding a pane, a plugin, or
wondering why something is in a strange place.

## The shape

```
watchers ──► store ──► panes ──► layout ──► screen
                        ▲
                     plugins
```

Watchers read clusters and publish snapshots into a store. Panes read the store and
return text. A layout engine decides where each pane goes. Nothing flows backwards.

## Panes are pure functions

A pane is `(snapshot, size, focus) → text`. It holds no state, caches nothing, and
is re-rendered whenever the store changes.

This is the decision most other things depend on. It makes panes testable from
literals — no cluster, no fakes, no informer machinery — and it is why the demo mode
exists at all: if a pane is a function of a store, then filling a store with invented
data produces a real dashboard rather than a mock of one. The demo replaces
*acquisition* and leaves rendering untouched, so a demo screenshot is evidence about
the real thing.

The rule it costs: a pane must render into exactly the rectangle it is given, and
clips rather than wraps. Wrapping would silently change a pane's line count and push
the grid off the bottom of the terminal.

## The store is typed and keyed

One map from key to typed snapshot, with a lock. A snapshot carries its items, when
it was updated, an error, and optionally a note — "still waiting, and here is what
for". That last field is why a pane can distinguish "nothing yet" from "nothing", and
say which.

Watchers coalesce: a rebuild re-projects every object of a kind, so one busy resource
must not drive that per event. Events on a management cluster mid-rollout are the
reason.

## Core versus plugin

**Core** is what needs nothing beyond Kubernetes, Cluster API and Metal3: clusters,
machines, hosts, nodes, pods, events. It is always present.

**Plugins** are optional subsystems — a CNI, a load balancer, storage, a cloud. Each
detects whether its prerequisites exist and contributes nothing at all when they do
not. That is what makes adding a plugin safe for people who do not run that
subsystem, and it is enforced by the boundary: core must not import a plugin.

A plugin can contribute four things, each optional:

- a **source** that publishes into the store,
- **panes**,
- **banner cells** for the health strip,
- a **summary block** for the overview.

The last one is how storage reports without a pane of its own: core defines a slot,
plugins fill it, and a cluster without that subsystem leaves it empty. Core still
knows nothing about Ceph — it receives rendered lines.

Because a group of panes may span plugins — a "network" frame holding two different
subsystems — grouping happens after every pane is known, not inside any plugin. No
plugin can see the others.

## Layout declares intent, not position

Panes say what they need and the packer places them:

- **priority** — which panes survive as the terminal narrows.
- **height weight** — relative pull on vertical space.
- **column span** — for tables whose *columns* do not fit. A pod name truncated to
  `rook-ceph/rook-c` is three different pods a reader cannot tell apart.
- **row span** — for tables whose *rows* do not fit. A fifty-node fleet shows
  twenty-five rows and `+ 25 more` no matter how wide the column is, because the
  limit is the row count.
- **grouping** — several panes under one frame.
- **stacking** — sharing a column.

Declaring intent rather than a position is what keeps the arrangement correct as
plugins come and go. A pane that exists only where one subsystem is installed cannot
be given a fixed slot, so the packer has to place it — and the same declarations
degrade from an ultrawide terminal down to a laptop without any pane knowing.

Two rules fell out of building it. Rows fill forward only, so a later low-priority
pane cannot backfill an earlier row's spare column and appear *above* a more
important one. And the last tile in a row grows into whatever is left, which absorbs
rounding, makes a lone tile full width, and closes the gap when a wide pane could not
fit — three jobs one rule does.

The renderer composites tiles by position, one output line at a time, rather than
joining rows. With row spans there are no rows left to join: a two-row pane sits
beside a different pane in each of the bands it covers.

## Themes are data

A theme is one struct — palette, chrome colors, frame glyphs, separators, and two
flags for whether chrome is capitalized and whether pane titles carry a tag. Applying
it rewrites the package-level styles every pane already drew from, so a theme reaches
the whole dashboard without a single pane knowing themes exist.

The hard part of a theme system is not the palette; it is **who owns each cell**.
Every bug in it was an unclaimed cell — a notch behind a glyph, an unpainted strip
beside a partial row, the space after a jump digit. None was visible until a theme
brought a background of its own. They are caught now by a test that walks each
rendered line tracking whether a background is armed.

## Where to look

| I want to… | Start at |
|---|---|
| add a core pane | `internal/core/panes` |
| add a plugin | `pkg/plugin` for the contract, `internal/plugin/*` for examples |
| change the layout engine | `pkg/tui/grid` |
| add a theme | `pkg/tui/theme.go` |
| change what a snapshot holds | `internal/core/model` |
| work on the demo fixture | `internal/demo` |
