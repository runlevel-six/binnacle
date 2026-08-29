package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/internal/build"
	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/kube"
	"github.com/runlevel-six/sextant/pkg/profile"
)

const kubeconfig = `apiVersion: v1
kind: Config
current-context: kind-capi-management
clusters:
  - name: mgmt
    cluster: {server: 'https://mgmt.test:6443', insecure-skip-tls-verify: true}
  - name: wl
    cluster: {server: 'https://wl.test:6443', insecure-skip-tls-verify: true}
contexts:
  - name: kind-capi-management
    context: {cluster: mgmt, user: admin}
  - name: prod-workload-a
    context: {cluster: wl, user: admin}
  - name: prod-workload-b
    context: {cluster: wl, user: admin}
users:
  - name: admin
    user: {token: t}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// --- profile selection ----------------------------------------------------

func TestSelectProfile(t *testing.T) {
	def, err := selectProfile("")
	if err != nil {
		t.Fatalf("empty name: %v", err)
	}
	if def.Name != "metal3" {
		t.Errorf("got %q want metal3", def.Name)
	}
	if _, err := selectProfile("metal3"); err != nil {
		t.Errorf("naming the default explicitly should work: %v", err)
	}
}

// An unknown profile is an error, not a silent fallback: quietly ignoring a
// requested profile would produce a dashboard that looks right and reports the
// wrong things.
func TestSelectProfile_UnknownIsAnError(t *testing.T) {
	_, err := selectProfile("no-such-site")
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), "no-such-site") {
		t.Errorf("error should name the profile: %v", err)
	}
}

// --- Prepare --------------------------------------------------------------

func TestPrepare_ZeroConfig(t *testing.T) {
	cfg := config.Config{KubeconfigPath: writeKubeconfig(t)}
	got, err := Prepare(cfg, build.Info{}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Resolved.ManagementContext != "kind-capi-management" {
		t.Errorf("management: got %q", got.Resolved.ManagementContext)
	}
	if !got.Resolved.WorkloadIsManagement {
		t.Error("workload should default to the management cluster")
	}
	if got.Store == nil {
		t.Error("Store should be initialized")
	}
}

func TestPrepare_UnknownProfile(t *testing.T) {
	cfg := config.Config{KubeconfigPath: writeKubeconfig(t), Profile: "nope"}
	if _, err := Prepare(cfg, build.Info{}, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPrepare_BadKubeconfigPath(t *testing.T) {
	cfg := config.Config{KubeconfigPath: filepath.Join(t.TempDir(), "absent")}
	if _, err := Prepare(cfg, build.Info{}, nil); err == nil {
		t.Fatal("expected an error for a missing explicit kubeconfig")
	}
}

// --- ListContexts ---------------------------------------------------------

func TestListContexts_MarksRoles(t *testing.T) {
	var sb strings.Builder
	cfg := config.Config{KubeconfigPath: writeKubeconfig(t)}
	if err := ListContexts(&sb, cfg); err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	out := sb.String()

	if !strings.Contains(out, "kind-capi-management") {
		t.Errorf("output missing the current context:\n%s", out)
	}
	// With no workload context configured, one cluster serves both roles.
	if !strings.Contains(out, "both") {
		t.Errorf("expected the selected context marked 'both':\n%s", out)
	}
	for _, name := range []string{"prod-workload-a", "prod-workload-b"} {
		if !strings.Contains(out, name) {
			t.Errorf("output missing %q:\n%s", name, out)
		}
	}
}

func TestListContexts_SeparateWorkload(t *testing.T) {
	var sb strings.Builder
	cfg := config.Config{
		KubeconfigPath: writeKubeconfig(t),
		Workload:       config.Cluster{Context: "prod-workload-a"},
	}
	if err := ListContexts(&sb, cfg); err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "mgmt") || !strings.Contains(out, "workload") {
		t.Errorf("expected both roles marked separately:\n%s", out)
	}
	if strings.Contains(out, "both") {
		t.Errorf("two distinct clusters should not be marked 'both':\n%s", out)
	}
}

// The listing is the tool for diagnosing a failed resolution, so it must still
// print the contexts when resolution fails.
func TestListContexts_ReportsResolutionFailureAndStillLists(t *testing.T) {
	var sb strings.Builder
	cfg := config.Config{
		KubeconfigPath: writeKubeconfig(t),
		Management:     config.Cluster{Context: "does-not-exist"},
	}
	if err := ListContexts(&sb, cfg); err != nil {
		t.Fatalf("ListContexts should not fail: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Resolution failed") {
		t.Errorf("expected the failure reported inline:\n%s", out)
	}
	if !strings.Contains(out, "prod-workload-a") {
		t.Errorf("contexts should still be listed:\n%s", out)
	}
}

func TestListContexts_EmptyKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := ListContexts(&sb, config.Config{KubeconfigPath: path}); err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if !strings.Contains(sb.String(), "No contexts found") {
		t.Errorf("got %q", sb.String())
	}
}

func TestSortedByName(t *testing.T) {
	got := sortedByName([]kube.ContextEntry{{Name: "z"}, {Name: "a"}, {Name: "m"}})
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "a,m,z" {
		t.Errorf("got %v want sorted", names)
	}
}

// --- store keys -----------------------------------------------------------

func TestStoreKeys_Sorted(t *testing.T) {
	cfg := config.Config{KubeconfigPath: writeKubeconfig(t)}
	s, err := Prepare(cfg, build.Info{}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	s.Store.Put("z/key", 1)
	s.Store.Put("a/key", 1)

	got := StoreKeys(s.Store)
	if len(got) != 2 || got[0] != "a/key" {
		t.Errorf("got %v want sorted", got)
	}
}

// Guards the assumption that the default profile is what an unconfigured run
// gets, since every zero-config claim depends on it.
func TestPrepare_UsesDefaultProfile(t *testing.T) {
	cfg := config.Config{KubeconfigPath: writeKubeconfig(t)}
	s, err := Prepare(cfg, build.Info{}, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := profile.Default()
	if s.Resolved.Profile.Name != want.Name {
		t.Errorf("got profile %q want %q", s.Resolved.Profile.Name, want.Name)
	}
	if got := s.Resolved.Profile.NodeRoles.RoleOf(map[string]string{
		"node-role.kubernetes.io/control-plane": "",
	}); got != "control-plane" {
		t.Errorf("default profile should read upstream role labels, got %q", got)
	}
}
