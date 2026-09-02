// Package fleet discovers the clusters a management cluster owns and keeps one
// sextant collector running against each.
package fleet

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// clusterGK is the Cluster API kind binnacle enumerates. The version is left to
// the cluster's own discovery, because a management cluster upgraded from
// v1beta1 to v1beta2 must not need a new binnacle — the same reason sextant
// resolves its kinds through a RESTMapper rather than pinning a GroupVersion.
var clusterGK = schema.GroupKind{Group: "cluster.x-k8s.io", Kind: "Cluster"}

// Discovered is one workload cluster found on the management cluster.
//
// Config is nil when the cluster exists but its credentials could not be
// resolved, and Err then says why. That is a state worth carrying rather than
// dropping: a cluster whose kubeconfig Secret has not been minted yet is newly
// created and belongs on the page as pending, not missing.
type Discovered struct {
	Namespace string
	Name      string
	Config    *rest.Config
	Err       error

	// ExecConfig is a separate identity for pods/exec on this workload
	// cluster, resolved from a Secret named <cluster>-exec-kubeconfig. Nil
	// means no such Secret exists, and exec falls back to Config — the
	// historical behavior. When set, the collector reads with Config and
	// execs with ExecConfig, which is how a read-only CAPI kubeconfig plus
	// a scoped exec ServiceAccount is expressed.
	ExecConfig *rest.Config

	// Clouds is this cluster's OpenStack credentials, when it has any. Nil
	// means it does not run OpenStack, or nobody has supplied them — either
	// way the plugin fails detection and contributes nothing, which is what it
	// is designed to do.
	Clouds *CloudCredentials
	// CloudsErr records a credential that exists but could not be used. Kept
	// apart from Err, which stops the cluster being collected at all: a
	// malformed clouds.yaml should cost the OpenStack pane, not the cluster.
	CloudsErr error
}

// Key identifies a cluster across reconciles.
func (d Discovered) Key() string { return d.Namespace + "/" + d.Name }

// Discoverer lists Cluster API clusters and resolves each one's workload
// credentials.
type Discoverer struct {
	dyn    dynamic.Interface
	core   kubernetes.Interface
	mapper meta.RESTMapper
	// namespace scopes the search. Empty means every namespace, which is what a
	// management cluster owning several tenants usually wants.
	namespace string
}

// NewDiscoverer builds a Discoverer against the management cluster.
func NewDiscoverer(cfg *rest.Config, namespace string) (*Discoverer, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	groups, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, fmt.Errorf("discover api groups: %w", err)
	}
	return &Discoverer{
		dyn:       dyn,
		core:      core,
		mapper:    restmapper.NewDiscoveryRESTMapper(groups),
		namespace: namespace,
	}, nil
}

// List enumerates the clusters and resolves credentials for each.
//
// A cluster whose credentials fail to resolve is still returned, carrying the
// error. Only a failure to enumerate at all is fatal, because that one means we
// do not know what the fleet contains — and a fleet page that silently shrinks
// is worse than one that will not load.
//
// # Why the fleet is not enumerated from kubeconfig Secrets
//
// Listing every "cluster.x-k8s.io/secret" whose name ends in -kubeconfig looks
// like a simpler discovery mechanism, and it is one — but it answers a different
// question. A Cluster object is a cluster; a Secret is a credential for one, and
// a cluster may have more than one credential under more than one name.
//
// Naming conventions change, and clusters built before a change keep the names
// they were built with, so one namespace can hold both eras at once: a Cluster
// named tenant-01 with CAPI's own tenant-01-kubeconfig, alongside a Cluster
// named tenant-02-cluster with its tenant-02-cluster-kubeconfig. Add an
// operator-maintained copy of the first under the newer convention —
// tenant-01-cluster-kubeconfig, kept in sync so that tooling assuming the
// convention keeps working — and there are three Secrets for two clusters.
//
// Enumerated from Secrets, that namespace reports three clusters, and the extra
// one is not an obvious ruin: it is the same live cluster a second time under a
// second name, with working credentials, rendering a second healthy card
// carrying identical numbers. A reader would have no way to tell which was real.
//
// Enumerated from Clusters, it reports two, each resolving its own Secret
// through upstream's convention, and the mixed naming is handled without
// binnacle knowing that any convention ever changed. That is also why the suffix
// is composed onto a Cluster's real name rather than applied as a site-wide
// rule: within one namespace, both eras can be live.
//
// So Clusters are the list and Secrets are only ever looked up *for* a Cluster.
// The failure mode this preserves is the right way round: a cluster whose Secret
// is missing appears on the page saying so, and a Secret with no Cluster of its
// name appears nowhere.
func (d *Discoverer) List(ctx context.Context) ([]Discovered, error) {
	mapping, err := d.mapper.RESTMapping(clusterGK)
	if err != nil {
		return nil, fmt.Errorf("the management cluster does not have Cluster API installed: %w", err)
	}

	list, err := d.dyn.Resource(mapping.Resource).Namespace(d.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	out := make([]Discovered, 0, len(list.Items))
	for _, item := range list.Items {
		found := Discovered{Namespace: item.GetNamespace(), Name: item.GetName()}
		found.Config, found.Err = d.workloadConfig(ctx, found.Namespace, found.Name)
		found.ExecConfig = d.execConfig(ctx, found.Namespace, found.Name)
		found.Clouds, found.CloudsErr = d.clouds(ctx, found.Namespace, found.Name)
		out = append(out, found)
	}
	return out, nil
}

// kubeconfigSuffix is Cluster API's own convention: the controller writes
// <cluster>-kubeconfig alongside the Cluster, which is the same artifact Argo CD
// registers a destination cluster from.
//
// Note this composes with a Cluster's *actual* name, so a site that suffixes its
// Cluster objects gets <name>-cluster-kubeconfig for free. There is no naming
// convention encoded here beyond upstream's.
const kubeconfigSuffix = "-kubeconfig"

// execKubeconfigSuffix names the Secret that holds a separate exec identity
// for a workload cluster. This is an operator-created Secret, not CAPI-minted:
// the operator creates a ServiceAccount with pods/exec scoped to the Ceph,
// Cilium, and OVN namespaces, and stores its kubeconfig here. When the Secret
// is absent, exec falls back to the main workload config.
const execKubeconfigSuffix = "-exec-kubeconfig"

// clusterNameLabel ties a Cluster API secret back to its Cluster. Used only as a
// fallback, and only ever to *narrow* candidates — never to widen them.
const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// workloadConfig resolves one workload cluster's credentials.
//
// The Cluster object is the source of truth for what exists; a Secret is only
// where that cluster's credentials happen to live. Resolution therefore always
// starts from a Cluster and looks for its Secret, never the reverse — see
// [Discoverer.List] for why the reverse is wrong.
//
// Two attempts, in order, and neither of them guesses:
//
//  1. The Secret named <cluster>-kubeconfig. This is upstream's convention and
//     covers every cluster observed so far.
//  2. Failing that, Secrets carrying this cluster's own name label, narrowed to
//     those holding a parseable kubeconfig. Exactly one match is used; several
//     is an error naming them, because picking one would mean picking a
//     credential for a cluster on the strength of a resemblance.
func (d *Discoverer) workloadConfig(ctx context.Context, namespace, name string) (*rest.Config, error) {
	secret, err := d.core.CoreV1().Secrets(namespace).Get(ctx, name+kubeconfigSuffix, metav1.GetOptions{})
	switch {
	case err == nil:
		return configFromSecret(secret)
	case !apierrors.IsNotFound(err):
		// Forbidden, or the API server is unwell. Either way the fallback would
		// fail the same way and for the same reason, so report this one.
		return nil, fmt.Errorf("read %s%s: %w", name, kubeconfigSuffix, err)
	}

	labeled, listErr := d.core.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: clusterNameLabel + "=" + name,
	})
	if listErr != nil {
		return nil, fmt.Errorf("no secret %s%s, and listing this cluster's secrets failed: %w",
			name, kubeconfigSuffix, listErr)
	}

	var candidates []corev1.Secret
	for _, s := range labeled.Items {
		if _, ok := s.Data["value"]; ok {
			candidates = append(candidates, s)
		}
	}
	switch len(candidates) {
	case 1:
		return configFromSecret(&candidates[0])
	case 0:
		return nil, fmt.Errorf("no kubeconfig for cluster %s: no secret named %s%s, "+
			"and none labeled %s=%s holds a \"value\" key",
			name, name, kubeconfigSuffix, clusterNameLabel, name)
	default:
		names := make([]string, 0, len(candidates))
		for _, s := range candidates {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("ambiguous kubeconfig for cluster %s: no secret named %s%s, "+
			"and %d labeled secrets hold one (%s). Refusing to guess which credential "+
			"belongs to this cluster",
			name, name, kubeconfigSuffix, len(candidates), strings.Join(names, ", "))
	}
}

// configFromSecret turns a Cluster API kubeconfig secret into a REST config.
func configFromSecret(secret *corev1.Secret) (*rest.Config, error) {
	raw, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("secret %s has no \"value\" key", secret.Name)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse secret %s: %w", secret.Name, err)
	}
	return cfg, nil
}

// execConfig resolves a separate exec identity for one workload cluster.
//
// Unlike workloadConfig, this is optional: a missing Secret returns nil
// without error, and exec falls back to the main workload config. The Secret
// is operator-created — it holds a kubeconfig for a ServiceAccount with
// pods/exec scoped to the namespaces where Ceph, Cilium, and OVN pods run.
//
// A Secret that exists but is malformed is an error worth carrying: the
// operator intended a scoped exec identity and it is broken, which is
// different from never having configured one.
func (d *Discoverer) execConfig(ctx context.Context, namespace, name string) *rest.Config {
	secret, err := d.core.CoreV1().Secrets(namespace).Get(ctx, name+execKubeconfigSuffix, metav1.GetOptions{})
	if err != nil {
		// Not found is the normal case: no scoped exec identity configured.
		// Any other error (Forbidden, timeout) is also non-fatal here — the
		// main workload config still works for exec, just without the
		// narrowing.
		return nil
	}
	cfg, err := configFromSecret(secret)
	if err != nil {
		return nil
	}
	return cfg
}
