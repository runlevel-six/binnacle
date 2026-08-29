package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/internal/kube"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/tui"
)

func contexts() []kube.ContextEntry {
	return []kube.ContextEntry{
		{Name: "current-mgmt", Current: true},
		{Name: "other-mgmt"},
		{Name: "prod-workload-a"},
		{Name: "prod-workload-b"},
	}
}

// --- the zero-config path -------------------------------------------------

// The whole design rests on this: no config file, no flags, no environment, and
// the current context is watched as both clusters.
func TestResolve_ZeroConfig(t *testing.T) {
	var c Config
	got, err := c.Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManagementContext != "current-mgmt" {
		t.Errorf("ManagementContext: got %q want current-mgmt", got.ManagementContext)
	}
	// The workload cluster defaults to the management cluster, so a self-hosted
	// or single-cluster setup still gets a node and pod view.
	if got.WorkloadContext != "current-mgmt" {
		t.Errorf("WorkloadContext: got %q want current-mgmt", got.WorkloadContext)
	}
	if !got.WorkloadIsManagement {
		t.Error("WorkloadIsManagement should be true when they are the same cluster")
	}
	// All namespaces, which is correct for upstream Cluster API.
	if len(got.ManagementNamespaces) != 0 {
		t.Errorf("ManagementNamespaces: got %v want empty", got.ManagementNamespaces)
	}
}

func TestResolve_ExplicitContexts(t *testing.T) {
	c := Config{
		Management: Cluster{Context: "other-mgmt"},
		Workload:   Cluster{Context: "prod-workload-a"},
	}
	got, err := c.Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManagementContext != "other-mgmt" || got.WorkloadContext != "prod-workload-a" {
		t.Errorf("got mgmt=%q workload=%q", got.ManagementContext, got.WorkloadContext)
	}
	if got.WorkloadIsManagement {
		t.Error("WorkloadIsManagement should be false for two different clusters")
	}
}

// Pointing both at the same context explicitly is still reported as one cluster.
func TestResolve_SameContextBothRoles(t *testing.T) {
	c := Config{
		Management: Cluster{Context: "other-mgmt"},
		Workload:   Cluster{Context: "other-mgmt"},
	}
	got, err := c.Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.WorkloadIsManagement {
		t.Error("WorkloadIsManagement should be true when both name the same context")
	}
}

// A config context outranks a profile's.
func TestResolve_ConfigBeatsProfile(t *testing.T) {
	prof := profile.Default()
	prof.Clusters.Management.Context = "other-mgmt"
	c := Config{Management: Cluster{Context: "current-mgmt"}}

	got, err := c.Resolve(contexts(), prof, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManagementContext != "current-mgmt" {
		t.Errorf("got %q want current-mgmt", got.ManagementContext)
	}
}

func TestResolve_ProfilePattern(t *testing.T) {
	prof := profile.Default()
	prof.Clusters.Management.ContextPattern = "^other-"

	var c Config
	got, err := c.Resolve(contexts(), prof, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManagementContext != "other-mgmt" {
		t.Errorf("got %q want other-mgmt", got.ManagementContext)
	}
}

func TestResolve_AmbiguousProfilePattern(t *testing.T) {
	prof := profile.Default()
	prof.Clusters.Workload.ContextPattern = "workload"

	var c Config
	_, err := c.Resolve(contexts(), prof, nil)
	var amb *kube.AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("got %v want AmbiguousError", err)
	}
}

// A workload cluster is only looked for when one is requested. Searching
// speculatively would turn an unrelated context into a second watched cluster.
func TestResolve_NoWorkloadSearchWhenUnrequested(t *testing.T) {
	picker := func(kube.Role, []kube.Candidate) (int, error) {
		t.Fatal("picker consulted for an unrequested workload cluster")
		return 0, nil
	}
	var c Config
	if _, err := c.Resolve(contexts(), profile.Default(), picker); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolve_UnknownManagementContext(t *testing.T) {
	c := Config{Management: Cluster{Context: "nope"}}
	if _, err := c.Resolve(contexts(), profile.Default(), nil); err == nil {
		t.Fatal("expected an error for an unknown context")
	}
}

func TestResolve_NamespacesFromConfigThenProfile(t *testing.T) {
	prof := profile.Default()
	prof.Clusters.Management.Namespaces = []string{"from-profile"}

	// Profile supplies them when the config does not.
	var c Config
	got, err := c.Resolve(contexts(), prof, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(got.ManagementNamespaces, ",") != "from-profile" {
		t.Errorf("got %v want [from-profile]", got.ManagementNamespaces)
	}

	// The config wins when both are set.
	c.Management.Namespaces = []string{"from-config"}
	got, err = c.Resolve(contexts(), prof, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Join(got.ManagementNamespaces, ",") != "from-config" {
		t.Errorf("got %v want [from-config]", got.ManagementNamespaces)
	}
}

// --- layering -------------------------------------------------------------

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFile(t *testing.T) {
	path := writeConfig(t, `
management:
  context: mgmt-from-file
  namespaces: [capi, capi-system]
workload:
  context: workload-from-file
profile: my-site
target_version: v1.33.0
kubeconfig: /tmp/kc
`)
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.Management.Context != "mgmt-from-file" || got.Workload.Context != "workload-from-file" {
		t.Errorf("contexts: got %+v", got)
	}
	if strings.Join(got.Management.Namespaces, ",") != "capi,capi-system" {
		t.Errorf("namespaces: got %v", got.Management.Namespaces)
	}
	if got.Profile != "my-site" || got.TargetVersion != "v1.33.0" || got.KubeconfigPath != "/tmp/kc" {
		t.Errorf("got %+v", got)
	}
}

// Running with no config file is the expected case, not a degraded one.
func TestLoadFile_MissingIsNotAnError(t *testing.T) {
	got, err := LoadFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// Config holds slices, so compare the fields rather than the struct.
	if got.Management.Context != "" || got.Workload.Context != "" || got.Profile != "" ||
		got.TargetVersion != "" || got.KubeconfigPath != "" ||
		len(got.Management.Namespaces) != 0 || len(got.Workload.Namespaces) != 0 {
		t.Errorf("got %+v want the zero Config", got)
	}
}

func TestLoadFile_EmptyPath(t *testing.T) {
	if _, err := LoadFile(""); err != nil {
		t.Fatalf("LoadFile(\"\"): %v", err)
	}
}

func TestLoadFile_MalformedYAML(t *testing.T) {
	path := writeConfig(t, "management: [this is not a mapping]")
	if _, err := LoadFile(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

// Flags win over the file, which is what MergeFile's under-layering provides.
func TestMergeFile_DoesNotOverrideSetFields(t *testing.T) {
	path := writeConfig(t, `
management:
  context: mgmt-from-file
profile: from-file
target_version: v1.file
`)
	c := Config{
		Management: Cluster{Context: "mgmt-from-flag"},
		Profile:    "from-flag",
	}
	if err := c.MergeFile(path); err != nil {
		t.Fatalf("MergeFile: %v", err)
	}
	if c.Management.Context != "mgmt-from-flag" {
		t.Errorf("flag should win: got %q", c.Management.Context)
	}
	if c.Profile != "from-flag" {
		t.Errorf("flag should win: got %q", c.Profile)
	}
	// An unset field is filled from the file.
	if c.TargetVersion != "v1.file" {
		t.Errorf("unset field should come from the file: got %q", c.TargetVersion)
	}
}

func TestMergeFile_NamespacesOnlyWhenUnset(t *testing.T) {
	path := writeConfig(t, "management:\n  namespaces: [from-file]\n")

	set := Config{Management: Cluster{Namespaces: []string{"from-flag"}}}
	if err := set.MergeFile(path); err != nil {
		t.Fatalf("MergeFile: %v", err)
	}
	if strings.Join(set.Management.Namespaces, ",") != "from-flag" {
		t.Errorf("got %v want [from-flag]", set.Management.Namespaces)
	}

	var unset Config
	if err := unset.MergeFile(path); err != nil {
		t.Fatalf("MergeFile: %v", err)
	}
	if strings.Join(unset.Management.Namespaces, ",") != "from-file" {
		t.Errorf("got %v want [from-file]", unset.Management.Namespaces)
	}
}

func TestMergeEnv(t *testing.T) {
	t.Setenv(EnvManagementContext, "mgmt-from-env")
	t.Setenv(EnvWorkloadContext, "workload-from-env")
	t.Setenv(EnvProfile, "env-profile")
	t.Setenv(EnvTargetVersion, "v1.env")

	var c Config
	c.MergeEnv()
	if c.Management.Context != "mgmt-from-env" || c.Workload.Context != "workload-from-env" {
		t.Errorf("got %+v", c)
	}
	if c.Profile != "env-profile" || c.TargetVersion != "v1.env" {
		t.Errorf("got %+v", c)
	}
}

func TestMergeEnv_DoesNotOverrideFlags(t *testing.T) {
	t.Setenv(EnvManagementContext, "mgmt-from-env")
	c := Config{Management: Cluster{Context: "mgmt-from-flag"}}
	c.MergeEnv()
	if c.Management.Context != "mgmt-from-flag" {
		t.Errorf("flag should win over env: got %q", c.Management.Context)
	}
}

// Full precedence chain: flag beats env beats file.
func TestPrecedence_FlagOverEnvOverFile(t *testing.T) {
	path := writeConfig(t, "management:\n  context: from-file\nprofile: file-profile\ntarget_version: v1.file\n")
	t.Setenv(EnvManagementContext, "from-env")
	t.Setenv(EnvProfile, "env-profile")

	// Mimic main: start from flags, then env, then file.
	c := Config{Management: Cluster{Context: "from-flag"}}
	c.MergeEnv()
	if err := c.MergeFile(path); err != nil {
		t.Fatalf("MergeFile: %v", err)
	}

	if c.Management.Context != "from-flag" {
		t.Errorf("context: got %q want from-flag", c.Management.Context)
	}
	if c.Profile != "env-profile" {
		t.Errorf("profile: got %q want env-profile (no flag set)", c.Profile)
	}
	if c.TargetVersion != "v1.file" {
		t.Errorf("target version: got %q want v1.file (no flag or env)", c.TargetVersion)
	}
}

// --- paths and example ----------------------------------------------------

func TestDefaultPath_HonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultPath(), filepath.Join("/xdg", "sextant", "config.yaml"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDefaultPath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/tester")
	if got, want := DefaultPath(), filepath.Join("/home/tester", ".config", "sextant", "config.yaml"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// The example must stay loadable, or `sextant init` would hand the user a file
// that fails to parse.
func TestExampleConfig_ParsesAndIsAllCommented(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatalf("WriteExample: %v", err)
	}

	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("the example config does not parse: %v", err)
	}
	// Every setting is commented out, so the example is equivalent to no
	// configuration at all — which must still resolve.
	if got.Management.Context != "" || got.Profile != "" || got.TargetVersion != "" {
		t.Errorf("example should set nothing, got %+v", got)
	}
	if _, err := got.Resolve(contexts(), profile.Default(), nil); err != nil {
		t.Errorf("the example config should resolve: %v", err)
	}
}

// --- OpenStack cloud selection --------------------------------------------

// OS_CLOUD is deliberately not SEXTANT_-prefixed: it is the OpenStack
// ecosystem's own variable, so an operator whose openstack CLI already works
// needs to export nothing extra.
func TestOSCloud_PrecedenceChain(t *testing.T) {
	path := writeConfig(t, "os_cloud: from-file\n")
	t.Setenv(EnvOSCloud, "from-env")

	// Flag beats both.
	flagged := Config{OSCloud: "from-flag"}
	flagged.MergeEnv()
	if err := flagged.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if flagged.OSCloud != "from-flag" {
		t.Errorf("got %q want from-flag", flagged.OSCloud)
	}

	// Env beats the file.
	var env Config
	env.MergeEnv()
	if err := env.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if env.OSCloud != "from-env" {
		t.Errorf("got %q want from-env", env.OSCloud)
	}

	// The file is the fallback.
	t.Setenv(EnvOSCloud, "")
	var file Config
	file.MergeEnv()
	if err := file.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if file.OSCloud != "from-file" {
		t.Errorf("got %q want from-file", file.OSCloud)
	}
}

func TestOSCloud_ReachesResolved(t *testing.T) {
	c := Config{OSCloud: "my-cloud"}
	got, err := c.Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.OSCloud != "my-cloud" {
		t.Errorf("got %q want my-cloud", got.OSCloud)
	}
}

// An unset cloud leaves the choice to the site profile's own plugin settings.
func TestOSCloud_UnsetIsEmpty(t *testing.T) {
	got, err := (&Config{}).Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.OSCloud != "" {
		t.Errorf("got %q want empty", got.OSCloud)
	}
}

// A management cluster owning several clusters has to be narrowed to the one
// being watched, and the name comes from the *resolved* workload context so a
// context pinned by flag yields the right cluster too.
func TestResolve_CAPIClusterNameFromWorkloadContext(t *testing.T) {
	entries := []kube.ContextEntry{
		{Name: "mgmt-01", Current: true},
		{Name: "site-tenant-01"},
		{Name: "site-tenant-02"},
	}
	prof := profile.Default()
	prof.Clusters.Workload.CAPINamePattern = `tenant-\d+`

	var c Config
	c.Management.Context = "mgmt-01"
	c.Workload.Context = "site-tenant-02"

	got, err := c.Resolve(entries, prof, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPIClusterName != "tenant-02" {
		t.Errorf("CAPIClusterName = %q, want tenant-02", got.CAPIClusterName)
	}
}

// With no workload cluster requested, the management context is the workload
// context, and the name is derived from it just the same.
func TestResolve_CAPIClusterNameSingleCluster(t *testing.T) {
	entries := []kube.ContextEntry{{Name: "site-tenant-01", Current: true}}
	prof := profile.Default()
	prof.Clusters.Workload.CAPINamePattern = `tenant-\d+`

	got, err := (&Config{}).Resolve(entries, prof, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPIClusterName != "tenant-01" {
		t.Errorf("CAPIClusterName = %q, want tenant-01", got.CAPIClusterName)
	}
}

// Nothing configured means no filter, which is the upstream case: one management
// cluster, one workload cluster, nothing to narrow.
func TestResolve_NoCAPIClusterNameByDefault(t *testing.T) {
	entries := []kube.ContextEntry{{Name: "kind-capi", Current: true}}
	got, err := (&Config{}).Resolve(entries, profile.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPIClusterName != "" {
		t.Errorf("CAPIClusterName = %q, want empty", got.CAPIClusterName)
	}
}

// --- theme ----------------------------------------------------------------

func TestTheme_PrecedenceChain(t *testing.T) {
	path := writeConfig(t, "theme: ansi\n")
	t.Setenv(EnvTheme, "lcars")

	// Flag beats both.
	flagged := Config{Theme: "default"}
	flagged.MergeEnv()
	if err := flagged.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if flagged.Theme != "default" {
		t.Errorf("got %q want default", flagged.Theme)
	}

	// Env beats the file.
	var env Config
	env.MergeEnv()
	if err := env.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if env.Theme != "lcars" {
		t.Errorf("got %q want lcars", env.Theme)
	}

	// The file is the fallback.
	t.Setenv(EnvTheme, "")
	var file Config
	file.MergeEnv()
	if err := file.MergeFile(path); err != nil {
		t.Fatal(err)
	}
	if file.Theme != "ansi" {
		t.Errorf("got %q want ansi", file.Theme)
	}
}

func TestTheme_ResolvesToATheme(t *testing.T) {
	got, err := (&Config{Theme: "lcars"}).Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Theme.Name != "lcars" {
		t.Errorf("Theme = %q, want lcars", got.Theme.Name)
	}

	// Unconfigured resolves to the default rather than to a zero theme, which
	// would render as an unstyled dashboard.
	plain, err := (&Config{}).Resolve(contexts(), profile.Default(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plain.Theme.Name != tui.DefaultTheme().Name {
		t.Errorf("Theme = %q, want %q", plain.Theme.Name, tui.DefaultTheme().Name)
	}
}

// A misspelled theme must fail before anything can prompt or connect: the point
// is to be told, not to stare at an unchanged dashboard.
func TestTheme_UnknownIsAnError(t *testing.T) {
	_, err := (&Config{Theme: "lcras"}).Resolve(contexts(), profile.Default(), nil)
	if err == nil {
		t.Fatal("an unknown theme should fail resolution")
	}
	if !strings.Contains(err.Error(), "lcras") {
		t.Errorf("error should name the bad theme: %v", err)
	}
}
