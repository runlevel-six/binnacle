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

Set `session-key` explicitly rather than letting binnacle generate one. A
generated key does not survive a restart, and sign-in then loops for anyone
whose session predates it.

Each deployment is independent, so each needs its own redirect URL registered
with the identity provider — one client with several redirect URIs, or one
client per site — and its own session key.

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
sees everything it can see. If that is more than you want, the shape of the
alternative is a read-only ServiceAccount on each workload cluster with its own
kubeconfig in a Secret binnacle resolves instead — same discovery, different
Secret.

## Ingress and Server-Sent Events

The fleet page is pushed over SSE, so the connection is long-lived by design.
A proxy read timeout shorter than the stream will cut it, and the failure is
quiet: the page keeps rendering whatever it last received and stops updating.
Binnacle sends a keepalive every 25 seconds; the annotations in
`binnacle.yaml` cover nginx and Traefik. Check the equivalent for whatever
ingress controller you run.
