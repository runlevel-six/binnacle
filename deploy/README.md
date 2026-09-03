# Deploying Binnacle

One binnacle per management cluster, running **on** that cluster. It discovers
the clusters its own management cluster owns, reads them with a ServiceAccount
there, and is reached through an Ingress on that cluster. Nothing routes
between sites.

## Why it has to run on the management cluster

The kubeconfig Cluster API mints for a workload cluster points at that
cluster's own control-plane endpoint — typically an internal VIP. That address
is routable from inside the management cluster and generally from nowhere else,
which is why binnacle run from a laptop reports the workload half as
unreachable while the management half answers normally. It is the same path
Argo CD uses to reach its destination clusters from the same management
cluster.

Running it locally against a real fleet therefore needs whatever network path
your operators use — a bastion, a VPN, a proxied kubeconfig context. For
looking at the interface rather than at real data, `binnacle --demo` needs no
cluster at all.

## Applying it

Two files, in order. Both carry placeholders you must change.

```
kubectl apply -f deploy/binnacle.yaml   # namespace, deployment, service, ingress
kubectl apply -f deploy/rbac.yaml       # service account, role, binding
```

A third file — `deploy/workload-rbac.yaml` — is applied to each workload
cluster, not the management cluster. See [Scoped exec on workload clusters](#scoped-exec-on-workload-clusters) below.

What to change first:

| Placeholder | Where | What it should be |
|---|---|---|
| `managed-clusters` | `rbac.yaml` (Role, RoleBinding), `binnacle.yaml` (`--namespace`) | The namespace your `Cluster` objects live in. Both must match. |
| `site-a` | `binnacle.yaml` (`--site`) | This management cluster's name, shown in the header and browser title. |
| `my-site` | `binnacle.yaml` (`--profile`) | Your sextant site profile. |
| `binnacle.site-a.example` | `binnacle.yaml` (Ingress host, `--oidc-redirect-url`) | The hostname this instance is served on. |
| `https://sso.example/realms/platform` | `binnacle.yaml` | Your OpenID Connect issuer. |
| `binnacle:latest` | `binnacle.yaml` | Your built image. |

The profile has to reach the pod. Mount it and point `--profile` at the path,
or bake it into the image; without it binnacle uses the built-in default, which
will report expected cordons as a standing drain on a fleet whose compute nodes
are cordoned by design.

## The secret

```
kubectl -n binnacle create secret generic binnacle-oidc \
  --from-literal=client-secret='<from your identity provider>' \
  --from-literal=session-key="$(head -c32 /dev/urandom | base64)"
```

`session-key` is any secret of at least 32 characters; base64 is what the line
above produces but is not required. Set it explicitly rather than letting
binnacle generate one — a generated key does not survive a restart, and sign-in
then loops for anyone whose session predates it.

Each deployment is independent, so each needs its own redirect URL registered
with the identity provider — one client with several redirect URIs, or one
client per site — and its own session key.

## OpenStack credentials

Each workload cluster is generally its own cloud, so credentials are per-cluster.
Binnacle looks for them in the namespace the `Cluster` objects live in:

1. A Secret named `<cluster>-clouds-yaml`.
2. Failing that, a Secret labeled `binnacle/clouds-yaml=<cluster>` — the escape
   hatch for a site that already names these something else. Two matches is a
   refusal naming both, because picking one would mean authenticating to a cloud
   on the strength of a resemblance, and that failure *succeeds*: it reports
   another cloud's inventory as this cluster's.

The Secret holds the file under `clouds.yaml`, and may name which entry inside
it to use under `cloud`. Without that key, `--os-cloud` or the site profile
decides.

```
kubectl -n managed-clusters create secret generic tenant-01-clouds-yaml \
  --from-file=clouds.yaml=./tenant-01-clouds.yaml \
  --from-literal=cloud=my-cloud
```

A cluster with no such Secret is not an error: the OpenStack plugin fails
detection and contributes nothing, which is what it is designed to do. A Secret
that exists but cannot be used *is* reported, on the card and on the cluster
page — a pane missing because nobody configured it and one missing because the
configuration is broken look identical, and only one of them is somebody's to
fix.

gophercloud reads credentials from a file and nothing else, so binnacle writes
each cluster's to `--clouds-dir`. The deployment mounts that as a memory-backed
`emptyDir`, so they never reach a disk, and each file is removed when its
collector stops.

## What it can read

On the management cluster, the Role grants read on Cluster API and Metal3
objects, events, and the Secrets holding each workload cluster's kubeconfig —
all in one namespace, which is why this is a Role and not a ClusterRole.

`list` on Secrets is only needed for the fallback that finds a cluster's
kubeconfig when it is not at the conventional `<cluster>-kubeconfig` name.
Dropping that verb is a supported narrowing: a cluster needing the fallback
then says so on its card instead of disappearing.

**The workload clusters are a different matter, and the Role does not describe
them.** Binnacle reads each one with the credential Cluster API minted for it,
which is conventionally a cluster-admin certificate. Binnacle only ever reads,
but the credential it holds is not a read-only one, and anyone who can sign in
sees everything it can see.

### Scoped exec on workload clusters

Three plugins — Ceph, Cilium, and OVN — run read-only status commands inside
pods (`ceph -s`, `cilium status`, `ovn-nbctl show`). Without `pods/exec`
permission these drop to informer-only tier, which is a thinner pane but not
an error. The tier reason says so and names `--server` as the fix.

The supported way to give the collector `pods/exec` without giving it
cluster-admin is a dedicated ServiceAccount on each workload cluster, with
exec scoped to the namespaces where those pods run. `deploy/workload-rbac.yaml`
defines this:

```
# On each workload cluster:
kubectl apply -f deploy/workload-rbac.yaml
```

Set the namespace placeholders (`rook-ceph`, `kube-system`, `ovn-kubernetes`)
to where each subsystem actually runs on that cluster. A subsystem not present
is simply not listed — its Role is not created, and the plugin falls back to
informer-only for it.

After applying the RBAC, create a long-lived token and store it as a kubeconfig
Secret on the management cluster:

```
# On the workload cluster — create a token that does not expire with the pod:
kubectl -n binnacle apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: binnacle-collector-token
  annotations:
    kubernetes.io/service-account.name: binnacle-collector
type: kubernetes.io/service-account-token
EOF

# Wait for the token to populate:
kubectl -n binnacle get secret binnacle-collector-token -o jsonpath='{.data.token}' | base64 -d

# On the management cluster — store the kubeconfig where binnacle resolves it.
# The Secret must be named <cluster>-exec-kubeconfig and carry the kubeconfig
# under "value", matching the convention for CAPI's own kubeconfig Secrets:
kubectl -n managed-clusters create secret generic tenant-01-exec-kubeconfig \
  --from-file=value=<(kubectl --kubeconfig=/path/to/workload.kubeconfig config view --raw)
```

Binnacle resolves `<cluster>-exec-kubeconfig` automatically. When the Secret is
absent, exec falls back to the CAPI-minted kubeconfig — the historical behavior.
When present, the collector reads with the CAPI kubeconfig and execs with the
scoped ServiceAccount, so the cluster-admin credential is never used for exec.

Human operators get a read-only role with no `pods/exec`. Under that role,
sextant sits at informer-only for Ceph, Cilium, and OVN **by design**. The
pane says so: *"no pods/exec permission — use --server for full detail."*
The server is the supported path to full-tier data.

### Per-user scoping

Once the collector holds exec privilege and human roles are read-only,
`--server` is the only path to full-tier Ceph, Cilium, and OVN detail. Without
scoping, every authenticated user sees exec-derived data for the entire fleet
— which means the RBAC posture hasn't actually improved, it just moved.

Scoping limits which clusters each user can see, based on their OIDC group
membership. It is opt-in: without a `--scope-file`, every authenticated user
sees the entire fleet (the historical behavior, and what a single-team
deployment wants).

Create a scope file mapping OIDC groups to namespaces:

```yaml
groups:
  platform-admins:
    - "*"               # sees everything
  site-a-ops:
    - site-a            # sees only clusters in the site-a namespace
    - site-a-infra
  site-b-ops:
    - site-b
```

Then point binnacle at it:

```
binnacle --oidc-issuer ... --scope-file /etc/binnacle/scopes.yaml
```

The namespace is the CAPI Cluster object's namespace on the management cluster.
A user in no mapped group sees nothing — which is a configuration error worth
making visible, rather than silently widening to everything.

Scoping applies to all routes: the HTML fleet page, cluster detail pages, the
JSON API, and the SSE stream. A cluster outside the user's scope returns 404,
not 403: *"there is no such cluster"* and *"you may not see this cluster"* must
not render differently, because revealing the existence of a hidden cluster
would defeat the scoping.

The storage panel is scoped by the reporting cluster's namespace. A Ceph
cluster visible from both site-a and site-b is visible to users scoped to
either; one reported only by site-b is hidden from users scoped to site-a.
The hardware inventory (BareMetalHosts) is datacenter-wide and cannot be
narrowed per-namespace — it is either shown or hidden with the storage cluster
it belongs to.

## Letting the terminal client in

`sextant --server` reads the same API the fleet page does, so it has to get past
the same authentication. It presents an OpenID Connect **ID token** as
`Authorization: Bearer`, which binnacle verifies against the issuer it already
trusts — same signature, audience and expiry checks as the browser session, and
the same `groups` claim feeds the same scoping.

**A terminal needs its own client.** The browser client is confidential: it
holds a secret, and a binary distributed to laptops cannot. So register a
*public* client for the CLI, with the device authorization grant enabled, and
tell binnacle its tokens are acceptable:

```
--oidc-cli-client-id=binnacle-cli
```

Without that flag binnacle accepts only `--oidc-client-id`'s audience, which is
correct for a deployment that registers one client and uses it for both.

Nothing here is specific to one provider. Any OpenID Connect issuer works; the
client needs to be public, allowed to issue ID tokens carrying whatever claim
your `--scope-file` keys on, and — for the nicest terminal experience —
permitted to use the device authorization grant (RFC 8628), which is what lets
an operator authenticate over SSH with no browser on the same machine.

**Clients discover all of this themselves.** `GET /api/v1/authinfo` is served
without authentication, necessarily: a client asks it precisely because it does
not yet hold a credential.

```console
$ curl -s https://binnacle.example/api/v1/authinfo | jq
{
  "auth_required": true,
  "issuer": "https://sso.example/realms/platform",
  "client_id": "binnacle-cli",
  "scopes": ["openid", "profile", "email"]
}
```

It publishes nothing the provider's own discovery document does not, plus the
one fact only this deployment knows: whether a credential is wanted at all. A
binnacle running without `--oidc-issuer` answers `"auth_required": false`, and
a terminal client then sends nothing — which is what keeps `--server` usable by
a deployment that has not adopted an identity provider.

## Ingress and Server-Sent Events

The fleet page is pushed over SSE, so the connection is long-lived by design.
A proxy read timeout shorter than the stream will cut it, and the failure is
quiet: the page keeps rendering whatever it last received and stops updating.
Binnacle sends a keepalive every 25 seconds; the annotations in
`binnacle.yaml` cover nginx and Traefik. Check the equivalent for whatever
ingress controller you run.
