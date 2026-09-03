# Connect to a binnacle server

Sextant normally reads a kubeconfig and shows one cluster. Pointed at a
[binnacle](https://github.com/runlevel-six/binnacle) server instead, it shows
the whole fleet — one line per cluster, worst first — and drills into any of
them for the full dashboard.

The useful part is that you need no cluster credentials of your own. The server
holds them; you authenticate to the server.

```
sextant --server https://binnacle.example
```

That is usually the entire configuration.

## Signing in

If the server has no identity provider in front of it, nothing happens: sextant
asks, is told no credential is wanted, and connects.

Otherwise it signs you in with the device grant. You will see something like:

```
Sign in to continue:
  https://sso.example/realms/platform/device?user_code=WDJB-MJHT

If that link does not open, visit https://sso.example/realms/platform/device
and enter code WDJB-MJHT
```

Open it in any browser — your laptop's, your phone's, it does not have to be
the machine sextant is running on. That is the point of this grant rather than
a browser redirect: sextant is often run over SSH or in a container, where
there is no browser to redirect *to*.

Approve it, and sextant continues on its own.

The token is saved under your user cache directory, so this happens once rather
than every run. When it expires, sextant renews it silently if it can and asks
again if it cannot.

### Signing out

Delete the saved tokens:

```
rm ~/.cache/sextant/tokens.json          # Linux
rm ~/Library/Caches/sextant/tokens.json  # macOS
```

## What the identity provider has to allow

Register a **public** client for the CLI — a distributed binary cannot keep a
client secret, and a public client is exactly the kind that does not need one.
Then enable the **device authorization grant** on it, and make sure it issues
**ID tokens** carrying whatever claim the server's scoping is keyed on
(commonly `groups`).

Tell binnacle that client's tokens are acceptable:

```
binnacle --oidc-cli-client-id=my-cli-client ...
```

Sextant discovers the rest — issuer, client id, scopes — from the server at
`/api/v1/authinfo`, so nothing about your provider is compiled in and nothing
needs configuring on each laptop.

## When the device grant is not an option

Some providers do not offer it, and some organisations issue tokens through
their own tooling. Two ways in:

**A token you already have**, which wins over everything else:

```
export SEXTANT_SERVER_TOKEN=<an ID token for the server's audience>
sextant --server https://binnacle.example
```

**A command that prints one**, in your config file — the same shape as
kubectl's credential plugins:

```yaml
server:
  url: https://binnacle.example
  token_command: ["my-idp-cli", "token", "--audience", "binnacle-cli"]
```

Anything that can write a token to stdout works. Sextant runs it, reads the
first line, and sends the result. If it fails, its standard error is reported
so you can see why.

## In scripts

Sextant offers an interactive sign-in only when stdin and stderr are both
terminals. Anywhere else — a pipeline, a CI job, a cron entry — it reports what
it needs rather than waiting for a browser nobody will open. Give those a
`SEXTANT_SERVER_TOKEN` or a `token_command`.

## Going straight to one cluster

```
sextant --server https://binnacle.example --server-cluster managed-clusters/tenant-03-cluster
```

Skips the fleet list. Esc still returns to it.

## When something is wrong

The fleet screen tells you rather than showing an empty list. An unreachable
server, an expired token, or a refused request each render as the reason. If
sextant has read the fleet before, the last good values stay on screen marked
`stale:` — old numbers are worth more than none, as long as nobody mistakes
them for current.

An expired token names its own remedy: restart sextant and it will sign you in
again.

## What does not change

Local mode. `sextant` with a kubeconfig behaves exactly as it always has — one
cluster, no fleet screen, and none of the above runs at all.
