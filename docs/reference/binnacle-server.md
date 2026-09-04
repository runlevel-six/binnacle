# binnacle server

Every flag, environment variable, page and endpoint the server has. For the
terminal client see [Command line](cli.md); for how to actually deploy this,
see [the deployment guide](../../deploy/README.md).

## Flags

`binnacle --help` prints this list; it is repeated here so you can read it
without a binary.

| Flag | Default | What it does |
|---|---|---|
| `--addr` | `127.0.0.1:8080` | Address to listen on. |
| `--kubeconfig` | *(empty)* | Path to a kubeconfig. Empty uses in-cluster credentials, then `$KUBECONFIG`. |
| `--management-context` | *(empty)* | Kubeconfig context for the management cluster. |
| `--namespace` | *(empty)* | Namespace to discover `Cluster` objects in. Empty means all. |
| `--management-name` | *(empty)* | What this management cluster is called locally, e.g. `admin-k8s00`. Empty renders it as "Management cluster". |
| `--profile` | *(empty)* | Site profile describing how these clusters are laid out. See [Site profiles](profiles.md). |
| `--site` | *(empty)* | The site or datacenter this instance watches, shown in the header and browser title. A label — not the profile, and not the management cluster's own name. |
| `--os-cloud` | *(empty)* | `clouds.yaml` entry to use for clusters whose own credentials do not name one. |
| `--clouds-dir` | *(empty)* | Where per-cluster `clouds.yaml` files are written. Empty uses a directory under the system temp dir. |
| `--oidc-issuer` | *(empty)* | OpenID Connect issuer URL, e.g. a Keycloak realm. |
| `--oidc-client-id` | *(empty)* | OpenID Connect client id. |
| `--oidc-cli-client-id` | *(empty)* | Client id for terminal clients, whose tokens are also accepted. Empty uses `--oidc-client-id`. |
| `--oidc-cli-scopes` | *(the browser's)* | Comma-separated scopes a terminal client should request. Add `offline_access` so a terminal session survives the provider's SSO idle timeout. Must include `openid`. |
| `--oidc-redirect-url` | *(empty)* | Binnacle's callback URL as the browser reaches it. |
| `--scope-file` | *(empty)* | YAML mapping OIDC groups to namespaces. Empty means no scoping — everyone sees everything. |
| `--allow-unauthenticated` | `false` | Serve without authentication on a non-loopback address. Every reader sees every cluster binnacle can read. |
| `--insecure-cookies` | `false` | Send session cookies without the `Secure` flag. For testing over plain HTTP only. |
| `--demo` | `false` | Serve an invented fleet. Needs no cluster and no credentials. |
| `--version` | `false` | Print the version and exit, before any credential is resolved. |

`--management-name` is worth setting even though nothing requires it. Readers
recognize `admin-k8s00` and have to stop and think about "Management cluster",
and it is the label on the one panel whose failure explains every stale card
below it. It is a flag rather than something discovered because Kubernetes has
no reliable notion of its own name: the kubeconfig context comes close and is
not dependably equal, and in a deployment there is no context at all — binnacle
takes in-cluster credentials. A name that is subtly wrong is worse than a
generic one that is honest.

Without `--oidc-issuer`, binnacle **refuses to start** on any address but
loopback unless `--allow-unauthenticated` is also given. It reads every cluster
in the fleet with credentials of its own, so an open listener is an open window
into all of them.

## Environment variables

Two, and neither has a flag, because a command line is visible in the process
table.

| Variable | What it holds |
|---|---|
| `BINNACLE_OIDC_CLIENT_SECRET` | The OIDC client secret. |
| `BINNACLE_SESSION_KEY` | The session signing key: any secret of 32+ characters, or base64 of 32+ bytes. Generated at startup if unset. |

Set the session key explicitly for more than one replica. Sessions signed by
one pod are rejected by the others, and the symptom is a sign-in that loops
rather than an error anyone can read.

## Pages

Each is a full page on first load and then updates over Server-Sent Events, so
none of them needs reloading while you watch.

| Page | What it holds |
|---|---|
| `/` | Every cluster as a card, worst first, plus the datacenter's storage layer and the management cluster's summary. |
| `/cluster/{namespace}/{name}` | One cluster in full: nodes, subsystems, network, cloud, unhealthy pods, node pools, machines, hardware, events. |
| `/management` | The management cluster itself — its unhealthy pods, the controllers every workload cluster depends on, its nodes, and the Cluster API events belonging to no workload cluster. It has no `Cluster` object, so it gets a page rather than a card. Titled with `--management-name` where set. |

`?display=wall` scales the whole page from one root font size, for a screen
somebody walks past. It is opt-in rather than detected: a 1920px viewport is as
likely to be a laptop at arm's length as a television.

Each page has a matching SSE endpoint that streams its body — `/events`,
`/cluster/{namespace}/{name}/events`, `/management/events`. These are what the
pages themselves consume; you would not normally call them.

## Read API

JSON, for [the terminal client](../how-to/connect-to-a-server.md) and anything
else that wants the same data. Every route is `GET`, and every route except
`/api/v1/authinfo` requires a credential — a session cookie or an
`Authorization: Bearer` ID token.

| Endpoint | Returns |
|---|---|
| `/api/v1/authinfo` | Whether authentication is required, and the issuer and client id to use. **Unauthenticated by necessity**: a client calls it precisely because it holds no credential yet. |
| `/api/v1/fleet` | Every cluster's summary, plus the storage layer. |
| `/api/v1/clusters/{namespace}/{name}` | One cluster's detail, as the cluster page renders it. |
| `/api/v1/clusters/{namespace}/{name}/snapshot` | That cluster's raw store contents, uncapped and uncurated. |
| `/api/v1/clusters/{namespace}/{name}/stream` | The same snapshot, pushed on every change. This is what hydrates a `sextant --server` dashboard. |
| `/api/v1/storage` | The datacenter's storage layer on its own. |
| `/api/v1/events` | The same payload as `/api/v1/fleet`, pushed on every change rather than polled. A client subscribes to this instead of asking on a timer. |

A request without a credential gets **401**, not a redirect to the login page.
Only browser navigations are redirected: a client that followed a 302 would
receive a login page with a 200 on it and have to guess that HTML was not the
fleet.

**The management cluster is not in this API yet.** `/api/v1/fleet` returns
workload clusters and storage, so a terminal client cannot render the management
section that the web page shows at [`/management`](#pages).

That is a gap rather than a boundary. Nothing about the design excludes it —
the data is collected, shaped and rendered already, and exposing it would be
another field on the fleet response plus a route. It simply has not been
prioritized, and the two front ends are not at parity until it is.

## Ungated routes

| Route | Why |
|---|---|
| `/healthz` | A probe cannot hold a credential. Returns 200 whenever the process is serving; it says nothing about whether any cluster is readable. |
| `/static/` | Stylesheet, favicon and marks. Cached for 24 hours. |

## Terminal sessions that outlive a meeting

`sextant --server` presents an ID token, renews it as it ages, and caches the
refresh token so a restart does not mean signing in again. Whether that survives
a gap depends on what the refresh token is tied to.

With the default scopes it is tied to the provider's **SSO session**, which
typically goes idle in half an hour. A dashboard that stays open renews often
enough to stay alive; one that is closed for an afternoon does not, and the next
run asks for a new sign-in.

`--oidc-cli-scopes=openid,profile,email,offline_access` changes that. An
offline token is not bound to the SSO session, so closing sextant and coming
back hours later works. It is deliberately not the default, and it is set on the
*terminal* scopes rather than the browser's, because the two want opposite
things: a browser session should end when the SSO session does.

**Bound it, in both places.** An offline token in a stock Keycloak realm is good
for 30 days of disuse, and indefinitely if used weekly.

- At the provider, cap the client's offline session — in Keycloak that is the
  client's Advanced settings, and an absolute cap may require the realm's
  "Offline Session Max Limited" to be on.
- On the machine, sextant applies its own ceiling: `server.max_session`,
  12 hours by default, measured from the sign-in. It holds whatever the provider
  is configured to allow, which is what makes it worth having.

`sextant --sign-out` revokes the session at the provider and removes the local
credential. With offline tokens that stops being housekeeping and starts being
the way a session actually ends, so it is worth telling people it exists.