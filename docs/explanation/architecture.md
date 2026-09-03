# Architecture

Why the code is shaped the way it is. Useful if you are adding a pane, a plugin, or
wondering why something is in a strange place.

## The shape

```
                                  ┌─► panes ──► layout ──► terminal   (sextant)
watchers ──► store ──► verdicts ──┤
                ▲                 └─► views ──► templates ──► HTML    (binnacle)
             plugins
```

Watchers read clusters and publish snapshots into a store. Verdicts turn those
snapshots into judgements — healthy, degraded, cordoned-and-that-is-fine. Two
front ends render the result. Nothing flows backwards.

## Two front ends, one data layer

This is the decision the repository is arranged around, and the reason both
binaries live in it.

The two programs answer the same questions about the same clusters. Both read
Cluster API, Metal3 and the subsystem plugins through the same collectors, and
both have to decide the same things about what they read: whether a cordoned
node is expected or alarming, whether a pod that is merely starting counts as
unhealthy, what a degraded subsystem means for a cluster overall. Only the last
step differs — one draws a terminal, the other a web page.

Kept apart, that overlap becomes two implementations of one judgement, and they
drift. The failure is not theoretical: an early version of the web front end
rendered `kube-proxy True` for a cluster where kube-proxy had been *replaced*,
because the interpretation lived in a terminal pane file rather than beside the
type it interpreted. The same cluster read healthy on one screen and not on the
other, with nothing to say which was right.

So the rule is: **a verdict lives in `pkg/health` or on the state type, never in
a renderer.** When a second consumer appears, the judgement moves to shared code
before anything else happens. `pkg/` is the seam — the data layer, the models
and the verdicts — and it is deliberately free of any terminal dependency, which
is checked rather than assumed: `go list -deps ./cmd/binnacle` names no
terminal library.

## `--server` is the same store, filled differently

A terminal client can read a cluster it has no credentials for by pointing at a
server: `sextant --server https://binnacle.example`.

What makes this cheap is that it is not a second code path. The server streams
its raw store contents as JSON, the client decodes them back into a store, and
the panes render from that store exactly as they render from a locally collected
one. A remote-mode plugin reports itself present and collects nothing, because
the collection already happened on the server.

Local mode is untouched and remains the default: sextant must keep working with
nothing but a kubeconfig, and there is no silent fallback between the two.

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
- **content width** — how much width the pane's *current data* can use.
- **content height** — the height past which it can only add blank lines.
- **grouping** — several panes under one frame.
- **stacking** — sharing a column.

The last two of those are why a row is not divided evenly. The panes in a row are
never equally hungry: Machines & Hosts wants 128 columns for a fleet with long
machine names, and the Cloud frame beside it wants 57. Four equal quarters of a
395-column terminal gave the first 99 — every machine name truncated to
`demo-workers-` — and left 42 blank in the second. Cells are divided in proportion to
what the panes say they can use instead, and rows are trimmed to what their panes can
fill, with the surplus going to the rows that never claim a ceiling because their row
count is the cluster's size. Panes that declare neither divide their row evenly, as
everything did before either existed.

A trimmed row is cut to its content plus a blank line above and below it, never to
the content exactly. The gutter inside a frame is spent from height the pane did not
use — the renderer moves the body down a line when there is room, which is free for a
pane the layout was generous to — so a tile cut to the last line its pane declared has
nothing left to spend and draws its first and last rows hard against the border. Two
rows bought back is what keeps a content-sized pane looking like the ones beside it.

Width is settled per *band* — a run of rows joined by a row-spanning tile — rather
than once for the whole grid. A tile spanning rows is one rectangle, so the rows it
covers have to agree on the boundaries it sits between; rows that are not joined
agree about nothing and are each sized for themselves.

Sizing from content means re-measuring on every store update, so several rules keep
the display still. They sit at three different points, because a dashboard can shift
in three different ways and no one rule reaches all of them: what a pane asks for,
when the screen accepts a new answer, and what a pane does inside the tile it has.

**What a pane asks for.** An appetite ignores its *transient* columns — a Metal3
error, the nodes still behind, a Kubernetes message, the newest object a rollup of
events happens to name — because what identifies a row changes when the fleet changes
and commentary changes every poll; charging Machines & Hosts for its HOST STATE cell
moved the pane by 37 cells on a twenty-second timer. And the layout is sized from
each pane's *highest recent* appetite rather than its current one, held for a couple
of minutes after the last time anything claimed it. Two states of a cluster can
honestly want different widths — a crash-looping pod with a 60-character name needs
room its neighbors are using, and stops needing it the moment the pod recovers — so
sizing from the instant reading makes each state starve whichever pane the other one
fed, and the screen swings for as long as the pod flaps. Holding the peak makes that
one move instead of one per poll, and every rule about *when* to redraw is downstream
of an input that will not sit still, so none of them can fix it.

**When the screen accepts a new answer.** The first measurement of a given terminal
is the one drawn. A later one is
adopted when the arrangement changed — a pane appeared, was hidden, or moved cell —
when a row's height changed by four lines or more, or when a tile has become too
narrow for its own content and the new layout would give it more. What it is
deliberately *not* adopted for is the ideal boundaries drifting. Width is shared out
in proportion to appetite, so one pane's content changing length re-proportions its
whole band whether or not anybody was short of room: a single crash-looping pod with
a long name moved every border on a four-column screen by up to 34 cells, and moved
them back when it recovered. A boundary that moves without relieving a truncation has
bought the reader nothing and cost them their place. A dashboard a few cells from
ideal reads better than one that fidgets.

The two directions are asked different questions because the panes answer different
questions: a width is a *want*, and a table reads correctly with more of it or less,
while a height is a *ceiling*, and a row that changes height is one where lines are
appearing or disappearing.

**What a pane does inside its tile.** A table sits behind a small edge pad so it is
not jammed against the border, and that pad is computed from the column *headers*,
never from the rows. Measured against the rows, one long cell took the pods table
past the threshold and slid every row of the pane four cells left, then back when the
pod recovered — inside a tile whose borders never moved, which is movement no layout
test sees and every reader does. The pad is held even when the rows would rather have
those cells: the table already knows how to give up width, and giving up four cells
of a stretch column beats moving the whole pane. A pane's declared appetite includes
the pad, so a tile sized to what a pane asked for still fits what it asked to show.

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
| change what a snapshot holds | `pkg/model` |
| change a health verdict | `pkg/health` — and nowhere else, see above |
| consume the data layer from outside the TUI | `pkg/collect` |
| read a subsystem's state from outside | `pkg/subsystem/*` |
| work on the terminal demo fixture | `internal/demo` |
| change what a web page shows | `internal/fleet` for the view, `internal/web/templates` for the markup |
| work on the fleet demo fixture | `internal/fleet/demo.go` |
| change the read API | `internal/web/api.go` |
| change the JSON the store streams as | `internal/wire` |
| work on `--server` mode | `internal/remote` for the client, `internal/app` for the wiring |
| change authentication | `internal/auth` for the server, `internal/clientauth` for the terminal client |

The two entry points are `cmd/sextant` and `cmd/binnacle`. Neither holds
logic — they parse flags and hand off.
