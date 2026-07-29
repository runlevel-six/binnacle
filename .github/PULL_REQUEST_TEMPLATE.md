<!-- Conventional-commit prefixes drive release notes: feat: / fix: / docs: / test: / chore: / ci: -->

## What this changes

<!-- One or two sentences. -->

## Why

<!-- The operational problem, or a link to the issue. -->

## Checklist

- [ ] `make check` passes (fmt, vet, test)
- [ ] `golangci-lint run` is clean
- [ ] Commits are signed off (`git commit -s`) — see [CONTRIBUTING.md](../CONTRIBUTING.md#developer-certificate-of-origin)
- [ ] No site-specific string literals added to core code (namespaces, label
      keys, workload names belong in a profile)
- [ ] If this touches an exec-based parser, fixtures were added or updated and
      the upstream version they came from is noted
- [ ] If this adds a plugin or pane, it is absent-safe on clusters without the
      underlying subsystem

## Tested against

<!-- Which cluster(s)? Which profile? "Not tested on real hardware" is a fine
     answer for pure-refactor PRs — just say so. -->
