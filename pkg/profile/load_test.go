package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loaderWith returns a Loader searching only dir, so tests are unaffected by any
// profiles on the machine running them.
func loaderWith(dir string) *Loader { return &Loader{Dirs: []string{dir}} }

func writeProfile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

// --- the zero-config path -------------------------------------------------

// An empty name must not depend on any file existing.
func TestLoad_EmptyNameReturnsDefault(t *testing.T) {
	got, err := loaderWith(t.TempDir()).Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != Default().Name {
		t.Errorf("got %q want %q", got.Name, Default().Name)
	}
}

// --- built-in profiles ----------------------------------------------------

// The shipped profiles must load, inherit and validate. If one is malformed the
// binary is broken for everyone who names it, so this is the most valuable test
// in the file.
func TestLoad_BuiltinProfiles(t *testing.T) {
	l := loaderWith(t.TempDir())
	for _, name := range []string{"metal3", "openstack"} {
		got, err := l.Load(name)
		if err != nil {
			t.Fatalf("built-in profile %q does not load: %v", name, err)
		}
		if got.Name != name {
			t.Errorf("%s: Name is %q", name, got.Name)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s does not validate: %v", name, err)
		}
	}
}

// The built-in metal3 profile must match the compiled-in Default, or the
// zero-config path and `--profile metal3` would behave differently.
func TestBuiltinMetal3MatchesDefault(t *testing.T) {
	got, err := loaderWith(t.TempDir()).Load("metal3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := Default()

	if strings.Join(got.NodeRoles.LabelKeys, ",") != strings.Join(def.NodeRoles.LabelKeys, ",") {
		t.Errorf("label keys differ: file %v vs Default %v",
			got.NodeRoles.LabelKeys, def.NodeRoles.LabelKeys)
	}
	if strings.Join(got.Events.Namespaces, ",") != strings.Join(def.Events.Namespaces, ",") {
		t.Errorf("event namespaces differ: file %v vs Default %v",
			got.Events.Namespaces, def.Events.Namespaces)
	}
	if strings.Join(got.Layout.Grid, ",") != strings.Join(def.Layout.Grid, ",") {
		t.Errorf("grid differs: file %v vs Default %v", got.Layout.Grid, def.Layout.Grid)
	}
	if !got.Clusters.Management.AllNamespaces() {
		t.Error("built-in metal3 should read all namespaces")
	}
}

// The openstack profile inherits from metal3, so it must carry the parent's
// upstream label fallback while adding its own conventions.
func TestLoad_BuiltinOpenStackInherits(t *testing.T) {
	got, err := loaderWith(t.TempDir()).Load("openstack")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Inherited from metal3.
	if role := got.NodeRoles.RoleOf(map[string]string{
		"node-role.kubernetes.io/control-plane": "",
	}); role != "control-plane" {
		t.Errorf("upstream label fallback not inherited: got %q", role)
	}
	// Its own additions.
	if !got.Events.Interesting("openstack") {
		t.Error("openstack namespace should be interesting")
	}
	if !got.Events.Interesting("kube-system") {
		t.Error("kube-system should still be interesting")
	}
	if len(got.CriticalWorkloads) == 0 {
		t.Error("expected pinned critical workloads")
	}
	// No plugin namespaces are pinned, and that is deliberate. Each plugin
	// discovers its own, and a pin would send it looking in the wrong place — on a
	// real cluster MetalLB was in kube-system rather than metallb-system, which a
	// pinned "metallb-system" would have broken.
	for name, block := range got.Plugins {
		if _, pinned := block["namespace"]; pinned {
			t.Errorf("plugin %q pins a namespace, defeating discovery: %v", name, block)
		}
	}
	if got.NodeRoles.DisplayName("compute") != "Compute" {
		t.Errorf("display name: got %q", got.NodeRoles.DisplayName("compute"))
	}
}

func TestAvailable_IncludesBuiltins(t *testing.T) {
	got := loaderWith(t.TempDir()).Available()
	for _, want := range []string{"metal3", "openstack"} {
		if !contains(got, want) {
			t.Errorf("Available %v should include %q", got, want)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// --- user profiles --------------------------------------------------------

func TestLoad_UserProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "mysite", `
name: mysite
node_roles:
  label_keys: [my-role]
events:
  namespaces: [my-namespace]
`)
	got, err := loaderWith(dir).Load("mysite")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.NodeRoles.RoleOf(map[string]string{"my-role": "compute"}) != "compute" {
		t.Error("user label key not applied")
	}
	if !got.Events.Interesting("my-namespace") {
		t.Error("user namespace not applied")
	}
}

// A user profile shadows a built-in of the same name, which is how a site
// customizes a shipped profile without forking the binary.
func TestLoad_UserProfileShadowsBuiltin(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "metal3", "name: metal3\nevents:\n  namespaces: [only-mine]\n")

	got, err := loaderWith(dir).Load("metal3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Events.Interesting("only-mine") {
		t.Error("user override not applied")
	}
	if got.Events.Interesting("kube-system") {
		t.Error("built-in should have been shadowed entirely, not merged")
	}
	// And it appears once in the listing, not twice.
	seen := 0
	for _, n := range loaderWith(dir).Available() {
		if n == "metal3" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("metal3 appears %d times in Available", seen)
	}
}

// A path-like name loads a file directly, so a one-off profile needs no install.
func TestLoad_ByPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oneoff.yaml")
	if err := os.WriteFile(path, []byte("name: oneoff\nevents:\n  all_namespaces: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loaderWith(t.TempDir()).Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "oneoff" || !got.Events.AllNamespaces {
		t.Errorf("got %+v", got)
	}
}

// A file whose name: is omitted takes its identity from the filename, so it
// cannot fail to load under the name it was requested by.
func TestLoad_NameDefaultsToFilename(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "unnamed", "events:\n  namespaces: [x]\n")
	got, err := loaderWith(dir).Load("unnamed")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "unnamed" {
		t.Errorf("Name: got %q want unnamed", got.Name)
	}
}

func TestLoad_UnknownProfile(t *testing.T) {
	_, err := loaderWith(t.TempDir()).Load("no-such-profile")
	var unknown *UnknownProfileError
	if !errors.As(err, &unknown) {
		t.Fatalf("got %v want UnknownProfileError", err)
	}
	// The message must tell the user what they can choose and where it looked.
	msg := unknown.Error()
	if !strings.Contains(msg, "metal3") {
		t.Errorf("error should list available profiles: %v", msg)
	}
	if !strings.Contains(msg, "searched") {
		t.Errorf("error should say where it looked: %v", msg)
	}
}

// --- inheritance ----------------------------------------------------------

func TestLoad_ExtendsChain(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "base", `
name: base
node_roles:
  label_keys: [base-role]
  display:
    a: Alpha
    b: Beta
events:
  namespaces: [base-ns]
`)
	writeProfile(t, dir, "middle", `
name: middle
extends: base
node_roles:
  display:
    b: Bravo
    c: Charlie
`)
	writeProfile(t, dir, "leaf", `
name: leaf
extends: middle
events:
  namespaces: [leaf-ns]
`)

	got, err := loaderWith(dir).Load("leaf")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "leaf" {
		t.Errorf("Name: got %q want leaf", got.Name)
	}
	// Label keys come all the way from base.
	if strings.Join(got.NodeRoles.LabelKeys, ",") != "base-role" {
		t.Errorf("label keys: got %v", got.NodeRoles.LabelKeys)
	}
	// Maps merge key by key across the whole chain.
	if got.NodeRoles.DisplayName("a") != "Alpha" {
		t.Error("base display name lost")
	}
	if got.NodeRoles.DisplayName("b") != "Bravo" {
		t.Error("middle should override base")
	}
	if got.NodeRoles.DisplayName("c") != "Charlie" {
		t.Error("middle addition lost")
	}
	// Slices replace, so the leaf narrows rather than appends.
	if got.Events.Interesting("base-ns") {
		t.Error("leaf should have replaced the inherited namespaces, not appended")
	}
	if !got.Events.Interesting("leaf-ns") {
		t.Error("leaf namespace missing")
	}
}

// A cycle must be reported with the path that produced it, not blow the stack.
func TestLoad_CycleDetected(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a", "name: a\nextends: b\n")
	writeProfile(t, dir, "b", "name: b\nextends: a\n")

	_, err := loaderWith(dir).Load("a")
	var cycle *CycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("got %v want CycleError", err)
	}
	if len(cycle.Chain) < 2 {
		t.Errorf("cycle should name the chain, got %v", cycle.Chain)
	}
}

func TestLoad_SelfExtendsIsACycle(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "loop", "name: loop\nextends: loop\n")
	var cycle *CycleError
	if _, err := loaderWith(dir).Load("loop"); !errors.As(err, &cycle) {
		t.Fatalf("got %v want CycleError", err)
	}
}

func TestLoad_MissingParent(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "orphan", "name: orphan\nextends: no-such-parent\n")
	_, err := loaderWith(dir).Load("orphan")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message should name both ends of the broken link.
	if !strings.Contains(err.Error(), "orphan") || !strings.Contains(err.Error(), "no-such-parent") {
		t.Errorf("error should name both profiles: %v", err)
	}
}

// --- Merge semantics ------------------------------------------------------

func TestMerge_PluginSettingsMergeKeyByKey(t *testing.T) {
	parent := Profile{Plugins: map[string]Settings{
		"ceph":   {"namespace": "rook-ceph", "tools_selector": "app=tools"},
		"cilium": {"namespace": "kube-system"},
	}}
	child := Profile{Name: "child", Plugins: map[string]Settings{
		"ceph": {"namespace": "storage"},
	}}

	got := Merge(parent, child)
	ceph := got.Plugins["ceph"]
	if ceph["namespace"] != "storage" {
		t.Errorf("child should override: got %v", ceph["namespace"])
	}
	if ceph["tools_selector"] != "app=tools" {
		t.Errorf("unmentioned setting should survive: got %v", ceph["tools_selector"])
	}
	if _, ok := got.Plugins["cilium"]; !ok {
		t.Error("an untouched plugin block should survive")
	}
}

func TestMerge_ScalarsOnlyOverrideWhenSet(t *testing.T) {
	parent := Profile{
		Name:        "parent",
		Description: "the parent",
		Clusters: Clusters{Management: ClusterRef{
			ContextPattern: "parent-pattern",
			Namespaces:     []string{"parent-ns"},
		}},
	}
	got := Merge(parent, Profile{Name: "child"})

	if got.Name != "child" {
		t.Errorf("identity comes from the child: got %q", got.Name)
	}
	if got.Description != "the parent" {
		t.Errorf("an unset description should inherit: got %q", got.Description)
	}
	if got.Clusters.Management.ContextPattern != "parent-pattern" {
		t.Error("an unset pattern should inherit")
	}
	if strings.Join(got.Clusters.Management.Namespaces, ",") != "parent-ns" {
		t.Error("unset namespaces should inherit")
	}
}

// AllNamespaces is one-way: a child may switch it on, and cannot switch a
// parent's off, because a false bool is indistinguishable from an absent one.
func TestMerge_AllNamespacesIsOneWay(t *testing.T) {
	on := Merge(Profile{}, Profile{Name: "c", Events: Events{AllNamespaces: true}})
	if !on.Events.AllNamespaces {
		t.Error("child should be able to enable AllNamespaces")
	}
	inherited := Merge(Profile{Events: Events{AllNamespaces: true}}, Profile{Name: "c"})
	if !inherited.Events.AllNamespaces {
		t.Error("parent's AllNamespaces should be inherited")
	}
}

// --- parsing --------------------------------------------------------------

// Strict decoding matters more than it looks: a silently ignored key produces a
// dashboard that starts and reports the wrong things.
func TestParse_UnknownFieldIsAnError(t *testing.T) {
	_, err := Parse([]byte("name: x\nnode_rolez: {}\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "node_rolez") {
		t.Errorf("error should name the offending field: %v", err)
	}
}

func TestParse_EmptyDocument(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatalf("an empty document should parse: %v", err)
	}
	if got.Name != "" {
		t.Errorf("got %+v want the zero Profile", got)
	}
}

func TestParse_MalformedYAML(t *testing.T) {
	if _, err := Parse([]byte("name: [unclosed\n")); err == nil {
		t.Fatal("expected a parse error")
	}
}

// --- validation -----------------------------------------------------------

// All problems are reported at once; fixing a config one error per run is a poor
// experience and the checks are independent.
func TestValidate_ReportsEveryProblem(t *testing.T) {
	p := Profile{
		NodeRoles: NodeRoles{
			LabelKeys:              []string{""},
			MachineDeploymentMatch: map[string][]string{"compute": {""}},
		},
		Events:            Events{NamespacePrefixes: []string{""}},
		CriticalWorkloads: []CriticalWorkload{{Kind: "StatefulSet"}},
		Clusters: Clusters{
			Management: ClusterRef{ContextPattern: "([unclosed"},
		},
	}
	err := p.Validate()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("got %v want ValidationError", err)
	}
	// name, empty label key, empty match pattern, empty prefix, workload name,
	// workload namespace, bad regex.
	if len(invalid.Problems) < 6 {
		t.Errorf("expected several problems at once, got %d:\n%v",
			len(invalid.Problems), invalid.Problems)
	}
	joined := strings.Join(invalid.Problems, "\n")
	for _, want := range []string{"name is empty", "label_keys", "namespace_prefixes", "regular expression"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems should mention %q:\n%s", want, joined)
		}
	}
}

// An empty prefix or pattern would match everything, which is a silent
// catastrophe rather than a harmless no-op.
func TestValidate_CatchesMatchEverythingPatterns(t *testing.T) {
	p := Default()
	p.Events.NamespacePrefixes = []string{""}
	if err := p.Validate(); err == nil {
		t.Error("an empty namespace prefix should be rejected")
	}

	p = Default()
	p.NodeRoles.MachineDeploymentMatch = map[string][]string{"compute": {""}}
	if err := p.Validate(); err == nil {
		t.Error("an empty MachineDeployment pattern should be rejected")
	}
}

func TestValidate_BareWildcardLabelKey(t *testing.T) {
	p := Default()
	p.NodeRoles.LabelKeys = []string{WildcardSuffix}
	if err := p.Validate(); err == nil {
		t.Error("a bare wildcard has no prefix to match and should be rejected")
	}
}

func TestValidate_LayoutStackReferences(t *testing.T) {
	tests := []struct {
		name   string
		layout Layout
		want   string
	}{
		{
			name: "host not placed",
			layout: Layout{
				Grid:  []string{"nodes"},
				Stack: map[string]StackSpec{"events": {Under: "ghost", Ratio: 0.4}},
			},
			want: "does not place",
		},
		{
			name: "stacked under itself",
			layout: Layout{
				Grid:  []string{"events"},
				Stack: map[string]StackSpec{"events": {Under: "events", Ratio: 0.4}},
			},
			want: "under itself",
		},
		{
			name: "ratio out of range",
			layout: Layout{
				Grid:  []string{"pods"},
				Stack: map[string]StackSpec{"events": {Under: "pods", Ratio: 1.5}},
			},
			want: "outside the range",
		},
		{
			name: "no host named",
			layout: Layout{
				Grid:  []string{"pods"},
				Stack: map[string]StackSpec{"events": {Ratio: 0.4}},
			},
			want: "no host pane",
		},
		{
			name: "duplicate pane",
			layout: Layout{
				TopRow: []string{"overview"},
				Grid:   []string{"overview", "nodes"},
			},
			want: "appears twice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			p.Layout = tc.layout
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q: %v", tc.want, err)
			}
		})
	}
}

func TestValidate_ValidStackPasses(t *testing.T) {
	p := Default()
	p.Layout = Layout{
		TopRow: []string{"overview"},
		Grid:   []string{"nodes", "pods"},
		Stack:  map[string]StackSpec{"events": {Under: "pods", Ratio: 0.45}},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("a consistent layout should validate: %v", err)
	}
}

func TestValidate_DefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("the built-in default must validate: %v", err)
	}
}

// A profile that fails validation must not be returned as usable.
func TestLoad_InvalidProfileIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "bad", "name: bad\nevents:\n  namespace_prefixes: ['']\n")
	if _, err := loaderWith(dir).Load("bad"); err == nil {
		t.Fatal("expected validation to reject the profile")
	}
}

// A file that exists but cannot be read is a real problem, and must not silently
// fall through to a built-in of the same name.
func TestLoad_UnreadableFileIsNotSilentlySkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "metal3.yaml")
	if err := os.WriteFile(path, []byte("name: metal3\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := loaderWith(dir).Load("metal3")
	if err == nil {
		t.Fatal("an unreadable profile should be an error, not a fallback")
	}
	var unknown *UnknownProfileError
	if errors.As(err, &unknown) {
		t.Errorf("should report the read failure, not 'unknown profile': %v", err)
	}
}

// The shipped profiles must not pin anything a plugin discovers for itself.
//
// This is a regression guard for a real mistake: an early openstack.yaml pinned
// metallb to metallb-system, and on the first cluster it met MetalLB was in
// kube-system. A pin does not merely fail to help — it overrides working discovery
// and sends the plugin somewhere wrong.
func TestBuiltinProfiles_DoNotPinDiscoverableSettings(t *testing.T) {
	l := loaderWith(t.TempDir())
	discovered := []string{"namespace", "speaker_name", "tools_selector", "daemonset_name"}

	for _, name := range l.Available() {
		p, err := l.Load(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for plugin, block := range p.Plugins {
			for _, key := range discovered {
				if _, pinned := block[key]; pinned {
					t.Errorf("profile %q pins %s.%s, which the plugin discovers for itself",
						name, plugin, key)
				}
			}
		}
	}
}
