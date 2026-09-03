# Themes

```sh
sextant --list-themes
sextant --theme ncurses
```

Set one with `--theme`, `SEXTANT_THEME`, or `theme:` in the config file, or press
`T` to cycle through them while running.

| Theme | Look |
|---|---|
| `default` | Green/amber/red on rounded borders. |
| `ansi` | The terminal's own sixteen colors, so it inherits your scheme. |
| `lcars` | LCARS-style console: black ground, block rails, amber and violet. |
| `ncurses` | DOS-era curses: blue panels, double-line boxes, white ink. |

Screenshots of each are in the [README](../../README.md#themes-sextant).

## What a theme may change

A theme sets the chrome and the status palette: border style, header band, the
green/amber/red meanings, glyphs, separators, and whether chrome text is
capitalized.

**A theme never rewrites data.** Field labels and pane titles may be shouted;
a context name, a cluster name, a pod name never is. Uppercasing an identifier
would be the interface reporting something false — `Prod-Tenant-01` is not the
name of anything — so the rule is enforced in one place rather than trusted to
each theme.

## Grounded themes

`lcars` and `ncurses` paint their own background — every cell, including the header
and footer — so they look the same on a light terminal as on a dark one. The other
two inherit whatever your terminal has.

For `lcars` this is not a preference. An LCARS panel is a colored block on an unlit
screen; the black between the rails *is* the display, so the look does not survive
being dropped on a pale terminal.

A grounded theme commits to all three of a panel color, a screen color and an ink
color. Half a ground is worse than none: unpainted cells read as damage rather than
as a choice, and text with no declared ink inherits whatever the terminal defaults
to, which on a light background can be nothing at all.

## A note on `lcars`

The theme is an homage assembled from color values and box-drawing characters. No
fonts, artwork, or images from any source are included, and none of the palette is
copied from a file — it is hex values in a struct literal.

This project is not affiliated with, endorsed by, or sponsored by CBS Studios or
Paramount. Star Trek and LCARS are the trademarks of their respective owners, used
here only to describe what the theme looks like.

## Adding one

A theme is one struct literal in `pkg/tui/theme.go` appended to the catalog. Every
entry point — `--theme`, `SEXTANT_THEME`, `theme:`, `--list-themes` and `T` — reads
that catalog, so nothing else needs touching, and the package's tests check every
theme in it for single-cell glyphs, a complete palette and a unique name.
