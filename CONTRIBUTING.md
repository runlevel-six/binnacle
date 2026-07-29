# Contributing to sextant

Thanks for your interest. sextant is a terminal dashboard for Cluster API on
bare metal, and the most valuable contributions right now are **reports from
clusters that aren't ours** — different Metal3 versions, different node-role
conventions, different CNI and storage stacks.

## Ways to help that don't involve writing Go

- **Tell us your cluster shape.** Open an issue describing your management
  cluster (CAPI version, Metal3/BMO version, how you label node roles, which
  namespaces your CAPI objects live in). Profile defaults are only as good as
  the range of clusters we know about.
- **Report a pane that renders wrong.** A screenshot plus the output of
  `sextant --debug-snapshot -v` is usually enough to diagnose it.
- **Contribute a profile.** If you got sextant working against your stack,
  `profiles/<yours>.yaml` is a genuinely useful contribution.

## Development

```sh
git clone https://github.com/runlevel-six/sextant.git
cd sextant
make check    # fmt + vet + test
make build    # ./sextant
```

Requires Go (see `go.mod` for the minimum). `make help` lists every target.

Before opening a PR, run `make check` and `golangci-lint run`. CI runs gofmt,
`go vet`, `go test -race`, golangci-lint, and a goreleaser dry-run.

## Architecture in one paragraph

Data sources (informers, exec-pollers, API clients) write immutable snapshots
into a keyed `Store`. Panes are stateless and read snapshots by key on every
`Render`. A grid layout engine decides which pane goes where for the current
terminal size. Sources and panes never reference each other directly — they meet
at a datastore key, which is the seam the plugin system is built on.

**The rule that matters:** core code may not contain a site-specific string
literal. Namespaces, label keys, workload names and role vocabularies belong in
a profile, not in Go. If you find yourself adding `"my-namespace"` to a core
file, it wants to be a profile field.

## Adding a plugin

Plugins are compiled in but must be invisible on clusters that don't have their
subsystem. A plugin provides a `Source` (which implements `Detect` so it can
skip itself silently) and optionally panes and health-banner cells.

Plugins that shell into a pod (`ceph -s`, `cilium status`) must degrade in three
tiers rather than erroring:

1. **Full detail** — exec works.
2. **Reduced** — no `pods/exec` permission; fall back to informer-visible data.
3. **Absent** — subsystem not installed; the pane leaves the catalog entirely.

Parsers for external command output must be table-tested against captured
fixtures, and should record which upstream versions the fixtures came from.
That output is an unversioned contract with a fast-moving project, and fixtures
are how we notice when it changes.

## Commit messages

Conventional-commit prefixes drive release-note grouping: `feat:`, `fix:`,
`docs:`, `test:`, `chore:`, `ci:`. Not enforced, but `feat:` and `fix:` land in
the changelog and everything else doesn't.

## Developer Certificate of Origin

Contributions require a DCO sign-off, matching the Cluster API and Metal3
projects. Add a `Signed-off-by` line with `git commit -s`:

```text
Signed-off-by: Your Name <your.email@example.com>
```

This certifies that you wrote the patch or have the right to submit it under
the project's license. Full text: <https://developercertificate.org/>

## License

By contributing, you agree your contributions are licensed under the
Apache License 2.0. See [LICENSE](LICENSE).
