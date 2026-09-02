# Security Policy

## Supported versions

sextant is pre-1.0. Only the latest tagged release receives fixes.

## Reporting a vulnerability

Please report security issues privately via GitHub's
[private vulnerability reporting][pvr] on this repository, rather than opening
a public issue.

[pvr]: https://github.com/runlevel-six/binnacle/security/advisories/new

Include what you can: affected version, reproduction steps, and impact. Expect
an initial response within a week — this is a small project, not a staffed
security team.

## Threat model

Useful context for judging whether something is a vulnerability here:

- **sextant is a read-only client.** It runs on an operator's workstation or
  jump host, reads from Kubernetes and (optionally) OpenStack APIs using
  credentials already present in `~/.kube/config` and `clouds.yaml`, and
  renders to a terminal. It performs no mutating API calls.
- **It requires `pods/exec` for some optional plugins.** Ceph, Cilium and OVN
  detail panes exec read-only status commands (`ceph -s`, `cilium status`,
  `ovn-appctl cluster/status`) inside existing pods. That permission is a
  meaningful grant — sextant only ever uses it for these fixed commands, never
  for user-supplied input, and every one of those plugins degrades to
  informer-only data when the permission is absent.
- **Credentials are never written or transmitted.** sextant does not phone
  home, emit telemetry, or persist anything from the clusters it reads.

Things we consider in scope: command injection into an exec'd command, leaking
credentials into terminal output or logs, parsing a hostile API response into a
crash or worse, and any path where sextant issues a mutating request.

Things we consider out of scope: the permissions your kubeconfig already
carries, and denial-of-service against your own cluster by pointing sextant at
it.
