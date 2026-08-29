package collect

import (
	"context"
	"testing"

	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

// --- namespace collapsing -------------------------------------------------

// An informer factory scopes to one namespace or to all of them, so a
// multi-namespace profile widens to cluster-wide. Widening is the safe direction:
// showing more than asked beats silently hiding objects.
func TestFirstNamespace(t *testing.T) {
	if got := firstNamespace(nil); got != "" {
		t.Errorf("nil: got %q want all-namespaces", got)
	}
	if got := firstNamespace([]string{"capi"}); got != "capi" {
		t.Errorf("single: got %q want capi", got)
	}
	if got := firstNamespace([]string{"capi", "capi-system"}); got != "" {
		t.Errorf("multiple should widen to all namespaces, got %q", got)
	}
}

// --- OpenStack cloud selection --------------------------------------------

// An explicit cloud — from --os-cloud or OS_CLOUD, resolved before it reaches
// here — overrides a profile's own setting, which is what an operator running
// several clouds needs: the profile describes how a cloud is laid out, the same
// for all of theirs, while the name says which one, and that changes per session.
func TestOpenStackSettings_OptionCloudOverridesProfile(t *testing.T) {
	pinned := map[string]profile.Settings{
		"openstack": {"cloud": "from-profile"},
	}

	// With nothing named, the profile's own setting stands.
	if got := openStackSettings(Options{Profile: profile.Profile{Plugins: pinned}}).Cloud; got != "from-profile" {
		t.Errorf("got %q want from-profile", got)
	}

	// A named cloud wins.
	opts := Options{Profile: profile.Profile{Plugins: pinned}, OSCloud: "my-cloud"}
	if got := openStackSettings(opts).Cloud; got != "my-cloud" {
		t.Errorf("got %q want my-cloud", got)
	}
}

// A profile that names no cloud and no flag leaves the plugin unconfigured, which
// is how it stays absent for the many users who do not run OpenStack.
func TestOpenStackSettings_UnconfiguredStaysEmpty(t *testing.T) {
	if got := openStackSettings(Options{}).Cloud; got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- option validation ----------------------------------------------------

// Watch takes its store and registry from the caller so that a consumer
// watching several clusters can hold one pair per cluster. Missing either is a
// programming error worth naming rather than a nil dereference three frames
// down inside an informer.
func TestWatch_RequiresStoreRegistryAndManagement(t *testing.T) {
	for name, opts := range map[string]Options{
		"no store":      {Registry: plugin.NewRegistry()},
		"no registry":   {Store: store.New()},
		"no management": {Store: store.New(), Registry: plugin.NewRegistry()},
	} {
		if err := Watch(context.Background(), opts, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Activate on an empty registry is a no-op rather than an error: a consumer
// that registers no plugins still gets the core watchers, and detection having
// nothing to say is not a failure.
func TestActivate_EmptyRegistry(t *testing.T) {
	if got := Activate(context.Background(), plugin.NewRegistry(), store.New()); len(got) != 0 {
		t.Errorf("got %d results, want none", len(got))
	}
}
