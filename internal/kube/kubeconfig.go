package kube

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Kubeconfig is loaded kubeconfig data plus the loading rules that produced it,
// so a REST config can be built for any context it contains.
type Kubeconfig struct {
	raw   *clientcmdapi.Config
	rules *clientcmd.ClientConfigLoadingRules
}

// Load reads kubeconfig data.
//
// When path is empty, client-go's standard loading rules apply: $KUBECONFIG
// (which may list several colon-separated files, merged in order), then
// ~/.kube/config. A non-empty path overrides both with a single file.
func Load(path string) (*Kubeconfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
		// Precedence would otherwise still be consulted and merged in.
		rules.Precedence = nil
	}

	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return &Kubeconfig{raw: raw, rules: rules}, nil
}

// Contexts returns every context in the kubeconfig.
//
// Order is unspecified — the underlying representation is a map. Callers that
// display contexts should sort; [Find] already does.
func (k *Kubeconfig) Contexts() []ContextEntry {
	out := make([]ContextEntry, 0, len(k.raw.Contexts))
	for name, ctx := range k.raw.Contexts {
		e := ContextEntry{
			Name:     name,
			Cluster:  ctx.Cluster,
			AuthInfo: ctx.AuthInfo,
			Current:  name == k.raw.CurrentContext,
		}
		if cl, ok := k.raw.Clusters[ctx.Cluster]; ok {
			e.Server = cl.Server
		}
		out = append(out, e)
	}
	return out
}

// CurrentContext returns the kubeconfig's current context name, which may be
// empty.
func (k *Kubeconfig) CurrentContext() string { return k.raw.CurrentContext }

// RestConfig builds a client-go REST config for a context.
//
// An empty contextName uses the current context. The returned config carries a
// user agent so cluster-side audit logs attribute requests to sextant.
func (k *Kubeconfig) RestConfig(contextName string) (*rest.Config, error) {
	if contextName != "" {
		if _, ok := k.raw.Contexts[contextName]; !ok {
			return nil, fmt.Errorf("kubeconfig has no context %q", contextName)
		}
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		k.rules,
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build REST config for context %q: %w", contextName, err)
	}
	return rest.AddUserAgent(cfg, "sextant"), nil
}
