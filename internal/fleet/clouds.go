package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// cloudsSuffix is the Secret name a cluster's clouds.yaml is looked for at.
	cloudsSuffix = "-clouds-yaml"
	// cloudsLabel finds that Secret when it is not at the conventional name.
	// Unlike the kubeconfig, this Secret is the operator's to create rather
	// than Cluster API's to mint, so the label is the escape hatch for a site
	// that already names these something else.
	cloudsLabel = "binnacle/clouds-yaml"
	// cloudsKey holds the file itself; cloudKey optionally names which entry
	// inside it to use.
	cloudsKey = "clouds.yaml"
	cloudKey  = "cloud"
)

// CloudCredentials is one cluster's OpenStack configuration.
type CloudCredentials struct {
	// CloudsYAML is the file's contents.
	CloudsYAML []byte
	// Cloud names the entry to use. Empty leaves the choice to the fleet-wide
	// --os-cloud or the site profile.
	Cloud string
	// Secret is where this came from, for diagnostics.
	Secret string
}

// clouds resolves one cluster's OpenStack credentials.
//
// Two attempts, neither of which guesses, and both of which are allowed to find
// nothing: a cluster with no clouds.yaml is a cluster that does not run
// OpenStack, or one whose credentials nobody has supplied yet. The plugin then
// fails detection and contributes nothing, which is the designed behavior — so
// absence is returned as (nil, nil) rather than as an error.
//
//  1. The Secret named <cluster>-clouds-yaml.
//  2. Failing that, Secrets labeled binnacle/clouds-yaml=<cluster>. Exactly one
//     is used; several is an error naming them, because picking one would mean
//     authenticating to a cloud on the strength of a resemblance.
func (d *Discoverer) clouds(ctx context.Context, namespace, name string) (*CloudCredentials, error) {
	secret, err := d.core.CoreV1().Secrets(namespace).Get(ctx, name+cloudsSuffix, metav1.GetOptions{})
	switch {
	case err == nil:
		return credentialsFrom(secret)
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("read %s%s: %w", name, cloudsSuffix, err)
	}

	labeled, err := d.core.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: cloudsLabel + "=" + name,
	})
	if err != nil {
		// Not fatal. Without list permission the conventional name still
		// works, and a cluster with no OpenStack credentials is a normal
		// state — refusing to collect anything at all over it would be a
		// wildly disproportionate response.
		return nil, nil //nolint:nilerr // absence is not a failure here
	}

	var found []corev1.Secret
	for _, s := range labeled.Items {
		if _, ok := s.Data[cloudsKey]; ok {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return credentialsFrom(&found[0])
	default:
		names := make([]string, 0, len(found))
		for _, s := range found {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("ambiguous OpenStack credentials for cluster %s: no secret named %s%s, "+
			"and %d labeled %s=%s hold a %q key (%s). Refusing to guess which cloud belongs to this cluster",
			name, name, cloudsSuffix, len(found), cloudsLabel, name, cloudsKey, strings.Join(names, ", "))
	}
}

func credentialsFrom(secret *corev1.Secret) (*CloudCredentials, error) {
	raw, ok := secret.Data[cloudsKey]
	if !ok {
		return nil, fmt.Errorf("secret %s has no %q key", secret.Name, cloudsKey)
	}
	return &CloudCredentials{
		CloudsYAML: raw,
		Cloud:      strings.TrimSpace(string(secret.Data[cloudKey])),
		Secret:     secret.Name,
	}, nil
}

// writeClouds puts a cluster's clouds.yaml where gophercloud can read it.
//
// A file, because that is what gophercloud takes: [clouds.Parse] accepts
// locations, not contents. Mode 0600 and a per-cluster name, and the deployment
// mounts this directory as a memory-backed emptyDir so the credentials never
// reach a disk.
func writeClouds(dir, key string, creds *CloudCredentials) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("clouds.yaml directory: %w", err)
	}
	// The key is namespace/name; a path separator in a filename is not what
	// anyone wants.
	path := filepath.Join(dir, strings.ReplaceAll(key, "/", "_")+".yaml")
	if err := os.WriteFile(path, creds.CloudsYAML, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
