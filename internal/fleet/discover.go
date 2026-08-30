// Package fleet discovers the clusters a management cluster owns and keeps one
// sextant collector running against each.
package fleet

import (
	"context"
	"fmt"

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
func (d *Discoverer) List(ctx context.Context) ([]Discovered, error) {
	mapping, err := d.mapper.RESTMapping(clusterGK)
	if err != nil {
		return nil, fmt.Errorf("Cluster API is not installed on the management cluster: %w", err)
	}

	list, err := d.dyn.Resource(mapping.Resource).Namespace(d.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	out := make([]Discovered, 0, len(list.Items))
	for _, item := range list.Items {
		found := Discovered{Namespace: item.GetNamespace(), Name: item.GetName()}
		found.Config, found.Err = d.workloadConfig(ctx, found.Namespace, found.Name)
		out = append(out, found)
	}
	return out, nil
}

// workloadConfig reads the kubeconfig Cluster API minted for a workload cluster.
//
// The Secret name and the "value" key are Cluster API's own convention: the
// controller writes <cluster>-kubeconfig alongside the Cluster, which is the
// same artifact Argo CD registers a destination cluster from. Reading it is why
// binnacle needs no hand-maintained cluster list — the list that goes stale is
// the one nobody updates.
func (d *Discoverer) workloadConfig(ctx context.Context, namespace, name string) (*rest.Config, error) {
	secret, err := d.core.CoreV1().Secrets(namespace).Get(ctx, name+"-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read %s-kubeconfig: %w", name, err)
	}
	raw, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("secret %s-kubeconfig has no \"value\" key", name)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s-kubeconfig: %w", name, err)
	}
	return cfg, nil
}
