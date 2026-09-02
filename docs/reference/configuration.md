# Configuration

Sextant needs no configuration. With no config file and no flags it watches your
kubeconfig's current context as both the management and the workload cluster, which
is right for a self-hosted or single-cluster setup.

Everything below is optional.

## Precedence

Three layers, highest first:

1. **Command-line flags.**
2. **Environment variables** — see [the variable list](cli.md#environment-variables).
3. **The config file.**

So a flag always beats an exported variable, and both beat the file. Nothing merges
field by field within a layer: if you pass `--management-context`, that is the
management context, whatever the file says.

Site conventions are a separate axis. A [site profile](profiles.md) describes how a
cluster is *laid out* — label keys, namespaces, pane arrangement. The config file
and flags describe *which* cluster and *how you like to look at it*. That split is
why the theme lives in config rather than in a profile: two people watching the
same site should not have to disagree in a shared file.

## Writing a starting file

```sh
sextant --init
```

That writes a commented example to `~/.config/sextant/config.yaml` with every key
present and switched off. Point `--config` elsewhere to keep several.

## Keys

```yaml
# The cluster running the Cluster API controllers.
management:
  context: capi-management
  # Namespaces holding Cluster API objects. Empty reads all of them, which is
  # correct for upstream Cluster API.
  namespaces: []

# Where nodes, pods and events are read from. Defaults to the management cluster.
workload:
  context: prod-workload

# Site profile: a name from --list-profiles, or a path to a YAML file.
profile: metal3

# The version you are rolling to. Setting it turns on rollout mode before the
# controllers have replaced anything.
target_version: v1.33.0

# Overrides $KUBECONFIG with a single file.
kubeconfig: ~/.kube/config

# Color scheme. See --list-themes.
theme: default

# Which clouds.yaml entry the OpenStack plugin should use.
os_cloud: my-cloud

# Fleet mode: connect to a binnacle server instead of reading a kubeconfig.
# Also settable with --server, --server-cluster, --token, or the SEXTANT_SERVER,
# SEXTANT_SERVER_CLUSTER, SEXTANT_SERVER_TOKEN environment variables.
server:
  url: http://binnacle:8080
  token: s3cr3t
  # Skip the fleet list and go straight to one cluster (namespace/name):
  # cluster: managed-clusters/tenant-03-cluster
```

Every key is optional, and an absent key is not the same as an empty one: omitting
`management.namespaces` reads every namespace, which is the upstream default.

## Checking what it resolved to

```sh
sextant --dry-run
```

That prints the management context, the workload context, the profile, the theme
and — when a profile narrows them — the Cluster API cluster name and OpenStack
cloud, then exits without starting anything. It is the fastest way to confirm a
config file is being read at all.
