package fleet

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testCloudsYAML = "clouds:\n  my-cloud:\n    auth:\n      auth_url: https://keystone.example/v3\n"

func cloudsSecret(namespace, name string, labels map[string]string, extra map[string][]byte) *corev1.Secret {
	data := map[string][]byte{"clouds.yaml": []byte(testCloudsYAML)}
	for k, v := range extra {
		data[k] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Data:       data,
	}
}

// The conventional name is the normal path, and it composes onto the Cluster's
// real name the same way the kubeconfig does.
func TestClouds_ByConventionalName(t *testing.T) {
	core := fake.NewSimpleClientset(cloudsSecret("capi", "tenant-01-clouds-yaml", nil, nil))
	d := discovererWith(core, fakeDynamic(), "capi")

	creds, err := d.clouds(context.Background(), "capi", "tenant-01")
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || !strings.Contains(string(creds.CloudsYAML), "keystone.example") {
		t.Fatalf("got %+v", creds)
	}
	if creds.Cloud != "" {
		t.Errorf("cloud = %q; nothing named one, so the fleet default should stand", creds.Cloud)
	}
}

// The secret may name which entry inside the file to use, which is what lets a
// site keep one naming scheme for files and another for the clouds in them.
func TestClouds_SecretMayNameTheEntry(t *testing.T) {
	core := fake.NewSimpleClientset(cloudsSecret("capi", "tenant-01-clouds-yaml", nil,
		map[string][]byte{"cloud": []byte("  my-cloud\n")}))
	d := discovererWith(core, fakeDynamic(), "capi")

	creds, err := d.clouds(context.Background(), "capi", "tenant-01")
	if err != nil || creds == nil {
		t.Fatalf("got %+v, %v", creds, err)
	}
	if creds.Cloud != "my-cloud" {
		t.Errorf("cloud = %q, want it trimmed to my-cloud", creds.Cloud)
	}
}

// Unlike the kubeconfig, this secret is the operator's to create, so the label
// is what lets a site that already names these something else be found.
func TestClouds_ByLabel(t *testing.T) {
	core := fake.NewSimpleClientset(cloudsSecret("capi", "site-credentials", map[string]string{
		cloudsLabel: "tenant-01",
	}, nil))
	d := discovererWith(core, fakeDynamic(), "capi")

	creds, err := d.clouds(context.Background(), "capi", "tenant-01")
	if err != nil || creds == nil {
		t.Fatalf("got %+v, %v", creds, err)
	}
	if creds.Secret != "site-credentials" {
		t.Errorf("secret = %q", creds.Secret)
	}
}

// Authenticating to a cloud on the strength of a resemblance is the worst
// outcome available here: it succeeds, and reports another cloud's inventory as
// this cluster's.
func TestClouds_AmbiguityIsRefused(t *testing.T) {
	labels := map[string]string{cloudsLabel: "tenant-01"}
	core := fake.NewSimpleClientset(
		cloudsSecret("capi", "one", labels, nil),
		cloudsSecret("capi", "two", labels, nil),
	)
	d := discovererWith(core, fakeDynamic(), "capi")

	_, err := d.clouds(context.Background(), "capi", "tenant-01")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"ambiguous", "one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A cluster with no credentials is not an error. It does not run OpenStack, or
// nobody has supplied them; either way the plugin fails detection and
// contributes nothing, which is what it is designed to do.
func TestClouds_AbsentIsNotAnError(t *testing.T) {
	d := discovererWith(fake.NewSimpleClientset(), fakeDynamic(), "capi")
	creds, err := d.clouds(context.Background(), "capi", "tenant-01")
	if err != nil || creds != nil {
		t.Errorf("got %+v, %v; want nothing and no error", creds, err)
	}
}

// A secret at the right name with no clouds.yaml in it is broken, not absent.
func TestClouds_SecretWithoutTheKeyIsAnError(t *testing.T) {
	core := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "capi", Name: "tenant-01-clouds-yaml"},
		Data:       map[string][]byte{"nope": []byte("x")},
	})
	d := discovererWith(core, fakeDynamic(), "capi")

	if _, err := d.clouds(context.Background(), "capi", "tenant-01"); err == nil ||
		!strings.Contains(err.Error(), "clouds.yaml") {
		t.Errorf("got %v; want an error naming the missing key", err)
	}
}

// gophercloud reads a file, so one gets written. It must not be world-readable
// and one cluster's must not overwrite another's.
func TestWriteClouds(t *testing.T) {
	dir := t.TempDir() + "/clouds"
	a, err := writeClouds(dir, "capi/tenant-01", &CloudCredentials{CloudsYAML: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := writeClouds(dir, "capi/tenant-02", &CloudCredentials{CloudsYAML: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two clusters wrote to the same file")
	}
	if strings.ContainsAny(strings.TrimPrefix(a, dir+"/"), "/") {
		t.Errorf("the cluster key leaked a path separator into the filename: %s", a)
	}

	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: these are credentials", perm)
	}
	got, _ := os.ReadFile(b)
	if string(got) != "b" {
		t.Errorf("contents = %q", got)
	}
}
