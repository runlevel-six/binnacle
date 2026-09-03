package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kubeconfig = `apiVersion: v1
kind: Config
current-context: mgmt
clusters:
  - name: c
    cluster: {server: 'https://mgmt.test:6443', insecure-skip-tls-verify: true}
contexts:
  - name: mgmt
    context: {cluster: c, user: u}
  - name: workload
    context: {cluster: c, user: u}
users:
  - name: u
    user: {token: t}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// runCLI invokes the CLI with a config path that does not exist, so tests are
// unaffected by any config file on the machine running them.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var sb strings.Builder
	err := run(context.Background(), args, &sb)
	return sb.String(), err
}

func TestVersionFlag(t *testing.T) {
	out, err := runCLI(t, "--version")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, version) || !strings.Contains(out, "sextant") {
		t.Errorf("got %q", out)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	out, err := runCLI(t, "--help")
	if err != nil {
		t.Fatalf("--help should not be an error: %v", err)
	}
	// Help must state the zero-config behavior, since that is the thing a new
	// user most needs to know.
	if !strings.Contains(out, "current context") {
		t.Errorf("help should explain the zero-config default:\n%s", out)
	}
	for _, want := range []string{"--management-context", "--debug-snapshot", "--list-contexts"} {
		if !strings.Contains(out, strings.TrimPrefix(want, "--")) {
			t.Errorf("help should mention %s:\n%s", want, out)
		}
	}
}

func TestUnknownFlagIsAnError(t *testing.T) {
	if _, err := runCLI(t, "--nope"); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

func TestListContexts(t *testing.T) {
	out, err := runCLI(t,
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--list-contexts",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "mgmt") || !strings.Contains(out, "workload") {
		t.Errorf("expected both contexts listed:\n%s", out)
	}
	if !strings.Contains(out, "CONTEXT") {
		t.Errorf("expected a header row:\n%s", out)
	}
}

// The zero-config run must resolve without any flags beyond the kubeconfig.
func TestDefaultRunResolvesWithoutConfiguration(t *testing.T) {
	out, err := runCLI(t, "--dry-run",
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "mgmt") {
		t.Errorf("expected the resolved context reported:\n%s", out)
	}
	if !strings.Contains(out, "metal3") {
		t.Errorf("expected the default profile reported:\n%s", out)
	}
}

func TestUnknownProfileIsAnError(t *testing.T) {
	_, err := runCLI(t,
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--profile", "does-not-exist",
	)
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
}

// Fleet mode resolves the profile too, and does it before the credential:
// signing in can open a browser and wait for a human, and a misspelled profile
// should not cost them that first. An unreachable server would also fail, so
// the assertion is on which failure comes back.
func TestServerModeRejectsAnUnknownProfileBeforeConnecting(t *testing.T) {
	_, err := runCLI(t,
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		// Port 1 is not listening; reaching it would be the other error.
		"--server", "http://127.0.0.1:1",
		"--profile", "does-not-exist",
	)
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("expected the profile named in the error, got: %v", err)
	}
}

func TestInitWritesExampleConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	out, err := runCLI(t, "--init", "--config", path)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, path) {
		t.Errorf("expected the written path reported: %q", out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

// --init is easy to run twice, and overwriting would discard real settings.
func TestInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("profile: mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "--init", "--config", path); err == nil {
		t.Fatal("expected --init to refuse to overwrite an existing file")
	}
	// And the original must be intact.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "profile: mine") {
		t.Error("--init overwrote an existing config")
	}
}

// A flag must beat the environment, which is the top of the precedence chain.
func TestFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("SEXTANT_MANAGEMENT_CONTEXT", "workload")
	out, err := runCLI(t, "--dry-run",
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--management-context", "mgmt",
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Resolved management context: mgmt") {
		t.Errorf("flag should win over the environment:\n%s", out)
	}
}

func TestEnvironmentIsHonoured(t *testing.T) {
	t.Setenv("SEXTANT_MANAGEMENT_CONTEXT", "workload")
	out, err := runCLI(t, "--dry-run",
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Resolved management context: workload") {
		t.Errorf("environment should be honored when no flag is set:\n%s", out)
	}
}

func TestConfigFileIsHonoured(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("management:\n  context: workload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "--dry-run", "--kubeconfig", writeKubeconfig(t), "--config", cfgPath)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Resolved management context: workload") {
		t.Errorf("config file should be honored:\n%s", out)
	}
}

func TestUnknownContextIsAnError(t *testing.T) {
	_, err := runCLI(t,
		"--kubeconfig", writeKubeconfig(t),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--management-context", "no-such-context",
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no-such-context") {
		t.Errorf("error should name the missing context: %v", err)
	}
}
