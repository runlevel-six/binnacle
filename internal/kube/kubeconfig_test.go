package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleKubeconfig = `apiVersion: v1
kind: Config
current-context: mgmt
clusters:
  - name: mgmt-cluster
    cluster:
      server: https://mgmt.example.test:6443
      insecure-skip-tls-verify: true
  - name: workload-cluster
    cluster:
      server: https://workload.example.test:6443
      insecure-skip-tls-verify: true
contexts:
  - name: mgmt
    context:
      cluster: mgmt-cluster
      user: mgmt-admin
  - name: workload
    context:
      cluster: workload-cluster
      user: workload-admin
users:
  - name: mgmt-admin
    user:
      token: mgmt-token
  - name: workload-admin
    user:
      token: workload-token
`

func writeKubeconfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestLoad_ExplicitPath(t *testing.T) {
	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := k.CurrentContext(); got != "mgmt" {
		t.Errorf("CurrentContext: got %q want mgmt", got)
	}

	got := k.Contexts()
	if len(got) != 2 {
		t.Fatalf("Contexts: got %d want 2", len(got))
	}
	byName := map[string]ContextEntry{}
	for _, e := range got {
		byName[e.Name] = e
	}

	mgmt, ok := byName["mgmt"]
	if !ok {
		t.Fatal("mgmt context missing")
	}
	if mgmt.Cluster != "mgmt-cluster" || mgmt.AuthInfo != "mgmt-admin" {
		t.Errorf("mgmt: got cluster=%q user=%q", mgmt.Cluster, mgmt.AuthInfo)
	}
	if mgmt.Server != "https://mgmt.example.test:6443" {
		t.Errorf("mgmt server: got %q", mgmt.Server)
	}
	if !mgmt.Current {
		t.Error("mgmt should be marked current")
	}
	if byName["workload"].Current {
		t.Error("workload should not be marked current")
	}
}

// An explicit path must not be merged with $KUBECONFIG, or a stray environment
// variable would silently add contexts the user did not ask for.
func TestLoad_ExplicitPathIgnoresEnv(t *testing.T) {
	other := writeKubeconfig(t, strings.ReplaceAll(sampleKubeconfig, "mgmt", "other"))
	t.Setenv("KUBECONFIG", other)

	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range k.Contexts() {
		if strings.HasPrefix(e.Name, "other") {
			t.Errorf("explicit path was merged with $KUBECONFIG: found %q", e.Name)
		}
	}
}

func TestLoad_HonoursKubeconfigEnv(t *testing.T) {
	path := writeKubeconfig(t, sampleKubeconfig)
	t.Setenv("KUBECONFIG", path)

	k, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(k.Contexts()) != 2 {
		t.Errorf("Contexts: got %d want 2", len(k.Contexts()))
	}
}

// An explicitly named file that does not exist is an error. The user asked for
// that specific file, so silently falling back to an empty config would hide
// the typo and produce a confusing "no contexts" message instead.
func TestLoad_MissingExplicitPathIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Load(missing); err == nil {
		t.Fatal("expected an error for a missing explicit kubeconfig path")
	}
}

// With no path given and no kubeconfig on disk, loading succeeds with an empty
// config. The resolver then produces an actionable "no contexts" message, which
// is more useful than a load failure for a file the user never named.
func TestLoad_NoKubeconfigAnywhereYieldsEmptyConfig(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty) // Windows equivalent

	k, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(k.Contexts()); got != 0 {
		t.Errorf("Contexts: got %d want 0", got)
	}

	// And the resulting error names the real problem.
	_, err = Resolve(k.Contexts(), RoleManagement, Selector{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no contexts") {
		t.Errorf("Resolve: got %v want a message about an empty kubeconfig", err)
	}
}

func TestRestConfig(t *testing.T) {
	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg, err := k.RestConfig("workload")
	if err != nil {
		t.Fatalf("RestConfig: %v", err)
	}
	if cfg.Host != "https://workload.example.test:6443" {
		t.Errorf("Host: got %q want the workload server", cfg.Host)
	}
	if cfg.BearerToken != "workload-token" {
		t.Errorf("BearerToken: got %q want workload-token", cfg.BearerToken)
	}
	// Audit logs should attribute requests to sextant.
	if !strings.Contains(cfg.UserAgent, "sextant") {
		t.Errorf("UserAgent: got %q want it to mention sextant", cfg.UserAgent)
	}
}

func TestRestConfig_EmptyNameUsesCurrentContext(t *testing.T) {
	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg, err := k.RestConfig("")
	if err != nil {
		t.Fatalf("RestConfig: %v", err)
	}
	if cfg.Host != "https://mgmt.example.test:6443" {
		t.Errorf("Host: got %q want the current context's server", cfg.Host)
	}
}

func TestRestConfig_UnknownContext(t *testing.T) {
	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = k.RestConfig("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown context")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the context: %v", err)
	}
}

// Load plus Resolve is the real startup path, so exercise them together.
func TestLoadAndResolve_EndToEnd(t *testing.T) {
	k, err := Load(writeKubeconfig(t, sampleKubeconfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	mgmt, err := Resolve(k.Contexts(), RoleManagement, Selector{}, nil)
	if err != nil {
		t.Fatalf("resolve management: %v", err)
	}
	if mgmt.Name != "mgmt" {
		t.Errorf("management: got %q want mgmt", mgmt.Name)
	}

	wl, err := Resolve(k.Contexts(), RoleWorkload, Selector{Context: "workload"}, nil)
	if err != nil {
		t.Fatalf("resolve workload: %v", err)
	}
	if wl.Name != "workload" {
		t.Errorf("workload: got %q want workload", wl.Name)
	}
}
