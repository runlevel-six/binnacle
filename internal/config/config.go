// Package config resolves the small number of inputs sextant needs at startup.
//
// The resolution order is flag, then environment variable, then config file,
// then default. Nothing is required: with no configuration at all, the
// kubeconfig's current context is watched as the management cluster and as the
// workload cluster, which is what makes the zero-config path work.
//
// Note what is absent compared with the predecessor tool, which required a
// datacenter name, an availability zone, and a cluster identifier drawn from one
// operator's naming scheme. None of that is meaningful anywhere else, so it is
// gone: a cluster is identified by its kubeconfig context, and everything
// site-specific lives in a profile.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/runlevel-six/binnacle/internal/kube"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// Environment variables, prefixed so they cannot collide with a shell that
// already exports generic names.
const (
	EnvManagementContext = "SEXTANT_MANAGEMENT_CONTEXT"
	EnvWorkloadContext   = "SEXTANT_WORKLOAD_CONTEXT"
	EnvProfile           = "SEXTANT_PROFILE"
	EnvTargetVersion     = "SEXTANT_TARGET_VERSION"
	EnvTheme             = "SEXTANT_THEME"
	EnvServerURL         = "SEXTANT_SERVER"
	EnvServerCluster     = "SEXTANT_SERVER_CLUSTER"
	EnvServerToken       = "SEXTANT_SERVER_TOKEN"

	// EnvOSCloud is deliberately *not* SEXTANT_-prefixed. OS_CLOUD is the
	// OpenStack ecosystem's own variable, read by the openstack CLI and every
	// SDK; an operator switching between clouds already exports it, and making
	// them export a second name for this tool alone would be gratuitous.
	EnvOSCloud = "OS_CLOUD"
)

// Config is the user-supplied configuration.
type Config struct {
	Management Cluster `yaml:"management"`
	Workload   Cluster `yaml:"workload"`

	// Profile names a profile to load. Empty selects the built-in default.
	Profile string `yaml:"profile,omitempty"`
	// TargetVersion is the Kubernetes version being rolled out. Setting it
	// activates rollout mode before the controllers have replaced anything.
	TargetVersion string `yaml:"target_version,omitempty"`
	// KubeconfigPath overrides $KUBECONFIG with a single file.
	KubeconfigPath string `yaml:"kubeconfig,omitempty"`
	// Theme names the color scheme. Empty selects the default. It is a config
	// field rather than a profile field because it says how one operator likes
	// to look at a cluster, not anything about the cluster itself — two people
	// watching the same site should not have to disagree in a shared profile.
	Theme string `yaml:"theme,omitempty"`
	// OSCloud names the clouds.yaml profile for the OpenStack plugin. It sits
	// here rather than only in a site profile because it identifies *which*
	// cloud, not how a cloud is laid out — an operator with several clouds
	// switches between them without editing a profile.
	OSCloud string `yaml:"os_cloud,omitempty"`
	// Server configures fleet mode: instead of reading a kubeconfig, sextant
	// connects to a binnacle deployment's JSON API. When URL is empty, all
	// three fields are ignored and the kubeconfig path is used.
	Server ServerConfig `yaml:"server,omitempty"`
}

// ServerConfig holds the connection details for fleet mode.
type ServerConfig struct {
	// URL is the root of the binnacle deployment (e.g. "http://binnacle:8080").
	URL string `yaml:"url,omitempty"`
	// Token is sent as a Bearer header. A server running with
	// --allow-unauthenticated does not need one.
	Token string `yaml:"token,omitempty"`
	// Cluster, when set, skips the fleet list and goes straight to this
	// cluster's detail view. Format is "namespace/name".
	Cluster string `yaml:"cluster,omitempty"`
}

// Cluster identifies one cluster to watch.
type Cluster struct {
	// Context pins a kubeconfig context name.
	Context string `yaml:"context,omitempty"`
	// Namespaces scopes what is read. Empty means all namespaces.
	Namespaces []string `yaml:"namespaces,omitempty"`
}

// Resolved is the outcome of resolving a Config against a kubeconfig and a
// profile. It is what the application wires its watchers from.
type Resolved struct {
	// ManagementContext is the context for Cluster API and Metal3 resources.
	ManagementContext string
	// WorkloadContext is the context for nodes, pods, events and workloads.
	WorkloadContext string
	// WorkloadIsManagement reports that both point at the same cluster, which
	// is the default. Worth surfacing in the header so a reader is not misled
	// into thinking two clusters are being watched.
	WorkloadIsManagement bool

	Profile       profile.Profile
	TargetVersion string
	// Theme is the resolved color scheme, already validated. The interface
	// applies it; nothing else needs to know a theme was chosen.
	Theme tui.Theme
	// OSCloud is the clouds.yaml profile to use, if one was named. Empty leaves
	// the choice to the site profile's own plugin settings.
	OSCloud string
	// ManagementNamespaces scopes the Cluster API read; empty means all.
	ManagementNamespaces []string
	// CAPIClusterName names the Cluster API cluster whose objects are watched.
	// Empty means every cluster in scope, which is right upstream where a
	// management cluster usually owns one.
	//
	// It comes from the *workload* cluster's profile entry, because that is the
	// cluster being watched: the Cluster object on the management side describes
	// the same cluster the workload context connects to.
	CAPIClusterName string
}

// DefaultPath returns the config file location, honoring XDG_CONFIG_HOME.
func DefaultPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "sextant", "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "sextant", "config.yaml")
	}
	return ""
}

// LoadFile reads a YAML config.
//
// A missing file is not an error: it returns the zero Config so a caller can
// layer flags and environment on top. Running with no config file is the
// expected case, not a degraded one.
func LoadFile(path string) (Config, error) {
	var c Config
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// MergeFile layers a file *under* c, filling only fields c has not set. Call it
// after flags are parsed so that flags win.
func (c *Config) MergeFile(path string) error {
	loaded, err := LoadFile(path)
	if err != nil {
		return err
	}
	setIfEmpty(&c.Management.Context, loaded.Management.Context)
	setIfEmpty(&c.Workload.Context, loaded.Workload.Context)
	setIfEmpty(&c.Profile, loaded.Profile)
	setIfEmpty(&c.TargetVersion, loaded.TargetVersion)
	setIfEmpty(&c.KubeconfigPath, loaded.KubeconfigPath)
	setIfEmpty(&c.OSCloud, loaded.OSCloud)
	setIfEmpty(&c.Theme, loaded.Theme)
	setIfEmpty(&c.Server.URL, loaded.Server.URL)
	setIfEmpty(&c.Server.Token, loaded.Server.Token)
	setIfEmpty(&c.Server.Cluster, loaded.Server.Cluster)
	if len(c.Management.Namespaces) == 0 {
		c.Management.Namespaces = loaded.Management.Namespaces
	}
	if len(c.Workload.Namespaces) == 0 {
		c.Workload.Namespaces = loaded.Workload.Namespaces
	}
	return nil
}

// MergeEnv layers environment variables under c, filling only unset fields.
func (c *Config) MergeEnv() {
	setIfEmpty(&c.Management.Context, os.Getenv(EnvManagementContext))
	setIfEmpty(&c.Workload.Context, os.Getenv(EnvWorkloadContext))
	setIfEmpty(&c.Profile, os.Getenv(EnvProfile))
	setIfEmpty(&c.TargetVersion, os.Getenv(EnvTargetVersion))
	setIfEmpty(&c.OSCloud, os.Getenv(EnvOSCloud))
	setIfEmpty(&c.Theme, os.Getenv(EnvTheme))
	setIfEmpty(&c.Server.URL, os.Getenv(EnvServerURL))
	setIfEmpty(&c.Server.Token, os.Getenv(EnvServerToken))
	setIfEmpty(&c.Server.Cluster, os.Getenv(EnvServerCluster))
}

// Resolve turns a Config plus a kubeconfig and profile into a [Resolved].
//
// The management context comes from the config, else the profile's pattern, else
// the kubeconfig's current context. The workload context falls back to the
// management context, so a self-hosted management cluster or a plain cluster with
// no Cluster API at all still yields a node and pod dashboard rather than an
// empty screen.
//
// picker may be nil, in which case an ambiguous pattern is an error naming the
// candidates.
func (c *Config) Resolve(entries []kube.ContextEntry, prof profile.Profile, picker kube.Picker) (Resolved, error) {
	// Checked first, and before anything that could prompt: a misspelled theme
	// should fail immediately rather than after the user has answered a context
	// picker.
	theme, err := tui.LookupTheme(c.Theme)
	if err != nil {
		return Resolved{}, err
	}

	mgmtSel := kube.Selector{
		Context: firstNonEmpty(c.Management.Context, prof.Clusters.Management.Context),
		Pattern: prof.Clusters.Management.ContextPattern,
	}
	mgmt, err := kube.Resolve(entries, kube.RoleManagement, mgmtSel, picker)
	if err != nil {
		return Resolved{}, err
	}

	out := Resolved{
		ManagementContext:    mgmt.Name,
		WorkloadContext:      mgmt.Name,
		WorkloadIsManagement: true,
		Profile:              prof,
		TargetVersion:        c.TargetVersion,
		Theme:                theme,
		OSCloud:              c.OSCloud,
		ManagementNamespaces: firstNonEmptySlice(c.Management.Namespaces, prof.Clusters.Management.Namespaces),
	}

	// A workload cluster is only resolved separately when one is actually
	// requested. Silently searching for one would turn an unrelated context
	// name into a second cluster nobody asked to watch.
	wlSel := kube.Selector{
		Context: firstNonEmpty(c.Workload.Context, prof.Clusters.Workload.Context),
		Pattern: prof.Clusters.Workload.ContextPattern,
	}
	if wlSel.IsZero() {
		out.CAPIClusterName = prof.Clusters.Workload.CAPINameFor(out.WorkloadContext)
		return out, nil
	}
	wl, err := kube.Resolve(entries, kube.RoleWorkload, wlSel, picker)
	if err != nil {
		return Resolved{}, err
	}
	out.WorkloadContext = wl.Name
	out.WorkloadIsManagement = wl.Name == mgmt.Name
	// Derived from the resolved context rather than from the pattern that found
	// it, so a context pinned by --workload-context yields the right cluster too.
	out.CAPIClusterName = prof.Clusters.Workload.CAPINameFor(wl.Name)
	return out, nil
}

func setIfEmpty(dst *string, src string) {
	if *dst == "" && src != "" {
		*dst = src
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptySlice(vals ...[]string) []string {
	for _, v := range vals {
		if len(v) > 0 {
			return v
		}
	}
	return nil
}

// ExampleConfig is a commented config file showing every field. Written by the
// init command so a user has something to edit rather than a blank page.
const ExampleConfig = `# sextant configuration. Every value here is optional, and any of them can be
# overridden by an environment variable or a command-line flag.
#
# With this file absent entirely, sextant watches your kubeconfig's current
# context as both the management and the workload cluster.

# The cluster running the Cluster API controllers.
management:
  # context: kind-capi-management
  # Namespaces holding Cluster API objects. Empty (the default) reads all of
  # them, which is correct for upstream Cluster API.
  # namespaces: []

# The cluster to read nodes, pods and events from. Defaults to the management
# cluster, which is what you want for a self-hosted or single-cluster setup.
# workload:
#   context: prod-workload

# Site conventions: node-role label keys, interesting namespaces, critical
# workloads, pane layout. Omit for the built-in Cluster API + Metal3 default.
# profile: metal3

# The Kubernetes version you are rolling out. Setting this turns on rollout mode
# before the controllers have replaced anything.
# target_version: v1.33.0

# Overrides $KUBECONFIG with a single file.
# kubeconfig: ~/.kube/config

# Color scheme. Run --list-themes for the current set. Also settable with
# --theme or SEXTANT_THEME, and cycled at runtime with T.
# theme: default

# The clouds.yaml profile for the OpenStack plugin. Also settable with --os-cloud
# or the standard OS_CLOUD environment variable, which take precedence over this.
# Useful here when you always watch the same cloud from this machine.
# os_cloud: my-cloud

# Fleet mode: connect to a binnacle server instead of reading a kubeconfig.
# Also settable with --server, --server-cluster, --token, or the SEXTANT_SERVER,
# SEXTANT_SERVER_CLUSTER, SEXTANT_SERVER_TOKEN environment variables.
# server:
#   url: http://binnacle:8080
#   token: s3cr3t
#   # Skip the fleet list and go straight to one cluster (namespace/name):
#   # cluster: managed-clusters/tenant-03-cluster
`

// WriteExample writes [ExampleConfig] to path, creating parent directories.
func WriteExample(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(ExampleConfig), 0o644)
}
