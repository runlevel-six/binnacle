<img src="assets/lockup.png" alt="Binnacle" width="360">

A fleet view of Cluster API clusters, in a browser.

Binnacle is the web front end to [sextant](https://github.com/runlevel-six/sextant).
It runs sextant's collectors, uses sextant's verdicts, and lays them out for
people who would rather open a tab than a terminal. One page shows every cluster
a management cluster owns: readiness, versions, replica counts, whether an
upgrade is in flight, and the same health indicators the dashboard shows.

## What it does differently from the TUI

**It watches the whole fleet at once.** The terminal dashboard is one operator
looking hard at one cluster. Binnacle lands on all of them, worst first, so the
cluster that needs attention is the one at the top of the page.

**It discovers what to watch.** There is no cluster list to maintain. Binnacle
reads the management cluster's own `Cluster` objects and resolves each one's
credentials from the `<cluster>-kubeconfig` Secret that Cluster API mints for
it. A cluster added this morning is on the page within a minute; one that is
deleted leaves it. The list that goes stale is the list nobody updates.

**It updates itself.** The page is server-rendered and pushed over Server-Sent
Events whenever the fleet moves. There is no refresh button, and no client-side
model that can drift out of step with the server's.

## What it deliberately does not do

Binnacle makes no judgements of its own. Whether a cordoned node is expected,
whether a Machine outside `Running` is a problem, which Raft member is entitled
to report a stale peer — all of that is decided in sextant's `pkg/health` and
arrives here already settled. A second opinion about a cluster's health is worse
than no second opinion: two tools that disagree leave an operator trusting
neither.

## Running it

Against your own kubeconfig, for development:

```
binnacle --management-context mgmt-01 --profile my-site
```

That listens on `127.0.0.1:8080` with no authentication, which is fine on a
machine only you are on. Binnacle **refuses to start** unauthenticated on any
other address: it reads every cluster in the fleet with credentials of its own,
so an open listener is an open window into all of them.

### Deployment shape

Manifests are in [`deploy/`](deploy/), with a `Dockerfile` at the root.

**One binnacle per management cluster, running on it.** Each instance discovers
the clusters its own management cluster owns, takes in-cluster credentials from a
ServiceAccount there, and is reached through an Ingress on that cluster. Nothing
has to route between sites.

```
binnacle \
  --addr :8080 \
  --site site-a \
  --namespace managed-clusters \
  --oidc-issuer https://sso.example/realms/platform \
  --oidc-client-id binnacle \
  --oidc-redirect-url https://binnacle.site-a.example/auth/callback
```

Set `--site`. Sites reuse workload cluster names, so two instances otherwise
render pages identical down to the names on the cards, and a tab strip shows
only the title. It is the one thing telling two open tabs apart.

Each deployment is independent, which means each needs its own OIDC redirect URL
registered with the provider — one client with several redirect URIs, or one
client per site — and its own `$BINNACLE_SESSION_KEY`. A session from one site
is not valid at another, which is correct: they are separate services that
happen to share a name.

The client secret comes from `$BINNACLE_OIDC_CLIENT_SECRET` and the session
signing key from `$BINNACLE_SESSION_KEY` (base64, at least 32 bytes). Neither is
a flag, because a command line is visible in the process table. Set the session
key explicitly for more than one replica: sessions signed by one pod are
rejected by the others, and the symptom is a sign-in that loops rather than an
error anyone can read.

### Access

Everyone who can sign in sees everything binnacle's ServiceAccount can see.
That is a deliberate simplification and it is the right trade for a status
board, but it is a real one: binnacle's RBAC, not the reader's, decides what is
on the page. Per-user impersonation — where the page shows exactly what that
person could see with `kubectl` — is the intended next step.

## Configuration

| Flag | What it does |
|---|---|
| `--addr` | Listen address. Default `127.0.0.1:8080`. |
| `--kubeconfig`, `--management-context` | Management cluster credentials. Omit both to use in-cluster credentials. |
| `--namespace` | Scope cluster discovery. Empty means every namespace. |
| `--site` | Names this instance in the header and browser title. Set it whenever more than one binnacle exists. |
| `--profile` | The sextant site profile describing how these clusters are laid out. |
| `--os-cloud` | The `clouds.yaml` entry to use for clusters whose own credentials do not name one. |
| `--clouds-dir` | Where per-cluster `clouds.yaml` files are written for gophercloud. Should be memory-backed. |
| `--oidc-issuer`, `--oidc-client-id`, `--oidc-redirect-url` | Turn on authentication. |
| `--insecure-cookies` | Send session cookies without `Secure`. Testing over plain HTTP only. |

## Required access

On the management cluster, read on `cluster.x-k8s.io` resources, the Metal3
kinds, and Events, plus `get` on `Secrets` in the namespaces the clusters live
in — that is where Cluster API keeps each workload cluster's kubeconfig. `list`
on those Secrets is needed only for the fallback that resolves a cluster whose
kubeconfig is not at the conventional name; without it, such a cluster reports
the reason on its card instead of disappearing. On each workload cluster, whatever the sextant plugins you want need
— nodes, pods and workloads at minimum. A plugin whose subsystem it cannot probe
contributes nothing rather than failing, so a narrow role degrades the page
instead of breaking it.
