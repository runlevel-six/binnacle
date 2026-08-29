package profile

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// builtinDir is where the embedded profiles live, relative to the repository
// root. Shipping them inside the binary means a user needs no files on disk to
// select a profile by name.
//
//go:embed all:builtin/*.yaml
var builtinFS embed.FS

const builtinDir = "builtin"

// maxExtendsDepth bounds inheritance. A chain longer than this is far more
// likely a mistake than a design, and the bound also stops a cycle that slipped
// past detection from looping forever.
const maxExtendsDepth = 16

// UnknownProfileError reports that a named profile could not be found.
type UnknownProfileError struct {
	Name string
	// Available lists the profiles that were found, so the message can show the
	// user what they can actually choose.
	Available []string
	// Searched lists the directories consulted, since "not found" is most often
	// a question of where the file was expected to be.
	Searched []string
}

func (e *UnknownProfileError) Error() string {
	msg := fmt.Sprintf("unknown profile %q", e.Name)
	if len(e.Available) > 0 {
		msg += fmt.Sprintf(" (available: %s)", strings.Join(e.Available, ", "))
	}
	if len(e.Searched) > 0 {
		msg += fmt.Sprintf("; searched %s", strings.Join(e.Searched, ", "))
	}
	return msg
}

// CycleError reports a loop in the extends chain.
type CycleError struct {
	Chain []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("profile inheritance cycle: %s", strings.Join(e.Chain, " → "))
}

// ValidationError reports a profile whose content is not usable.
type ValidationError struct {
	Profile  string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("profile %q is invalid:\n  - %s",
		e.Profile, strings.Join(e.Problems, "\n  - "))
}

// Loader finds and resolves profiles.
//
// Profiles are looked up by name across the user's search directories first and
// the embedded set second, so a site can override a shipped profile by dropping
// a file of the same name into its config directory. A name containing a path
// separator or ending in .yaml is treated as a literal file path instead, which
// is what makes a one-off profile easy to try without installing it.
type Loader struct {
	// Dirs are searched in order before the embedded profiles.
	Dirs []string
}

// NewLoader returns a Loader searching the conventional user directories.
func NewLoader() *Loader {
	var dirs []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		dirs = append(dirs, filepath.Join(x, "sextant", "profiles"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "sextant", "profiles"))
	}
	// A ./profiles directory makes a checkout's own profiles work without
	// installation, which is how contributors will try each other's.
	dirs = append(dirs, "profiles")
	return &Loader{Dirs: dirs}
}

// Load resolves a profile by name or path, applying inheritance and validating
// the result.
//
// An empty name returns [Default] — the zero-config path must not depend on any
// file being present.
func (l *Loader) Load(name string) (Profile, error) {
	if name == "" {
		return Default(), nil
	}

	resolved, err := l.load(name, nil)
	if err != nil {
		return Profile{}, err
	}
	if err := resolved.Validate(); err != nil {
		return Profile{}, err
	}
	return resolved, nil
}

// load resolves name, following extends. chain carries the names already visited
// so a cycle is reported with the path that produced it rather than as a stack
// overflow.
func (l *Loader) load(name string, chain []string) (Profile, error) {
	for _, seen := range chain {
		if seen == name {
			return Profile{}, &CycleError{Chain: append(append([]string{}, chain...), name)}
		}
	}
	if len(chain) >= maxExtendsDepth {
		return Profile{}, &CycleError{Chain: append(append([]string{}, chain...), name)}
	}

	data, source, err := l.read(name)
	if err != nil {
		return Profile{}, err
	}

	child, err := Parse(data)
	if err != nil {
		return Profile{}, fmt.Errorf("parse %s: %w", source, err)
	}
	// A profile's identity is the name it was requested by, so a file whose
	// `name:` disagrees with its filename cannot be selected under a name that
	// then fails to load.
	if child.Name == "" {
		child.Name = strings.TrimSuffix(filepath.Base(name), ".yaml")
	}

	if child.Extends == "" {
		return child, nil
	}
	parent, err := l.load(child.Extends, append(chain, name))
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q extends %q: %w", child.Name, child.Extends, err)
	}
	return Merge(parent, child), nil
}

// read locates a profile's bytes, returning them with a description of where
// they came from for error messages.
func (l *Loader) read(name string) ([]byte, string, error) {
	// A path-like name is a literal file, so a profile can be tried from
	// anywhere without being installed first.
	if strings.ContainsRune(name, filepath.Separator) || strings.HasSuffix(name, ".yaml") {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, "", fmt.Errorf("read profile %s: %w", name, err)
		}
		return data, name, nil
	}

	filename := name + ".yaml"
	for _, dir := range l.Dirs {
		path := filepath.Join(dir, filename)
		data, err := os.ReadFile(path)
		if err == nil {
			return data, path, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// A file that exists but cannot be read is a real problem — a
			// permission mistake, say — and silently falling through to the
			// embedded copy would hide it.
			return nil, "", fmt.Errorf("read profile %s: %w", path, err)
		}
	}

	if data, err := builtinFS.ReadFile(builtinDir + "/" + filename); err == nil {
		return data, "builtin:" + filename, nil
	}

	return nil, "", &UnknownProfileError{
		Name:      name,
		Available: l.Available(),
		Searched:  append(append([]string{}, l.Dirs...), "builtin"),
	}
}

// Available lists every profile name that can be loaded, deduplicated and
// sorted. A user-supplied profile shadowing a built-in appears once.
func (l *Loader) Available() []string {
	seen := map[string]bool{}
	add := func(filename string) {
		if strings.HasSuffix(filename, ".yaml") {
			seen[strings.TrimSuffix(filename, ".yaml")] = true
		}
	}

	for _, dir := range l.Dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a missing profile directory is normal
		}
		for _, e := range entries {
			if !e.IsDir() {
				add(e.Name())
			}
		}
	}
	if entries, err := builtinFS.ReadDir(builtinDir); err == nil {
		for _, e := range entries {
			add(e.Name())
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Parse decodes one profile document.
//
// Decoding is strict: an unrecognized field is an error rather than being
// ignored. A silently dropped key is the worst failure mode for configuration
// like this, because the dashboard still starts and simply reports the wrong
// things — a typo in a label key would look like "no nodes have roles".
func Parse(data []byte) (Profile, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	var p Profile
	if err := dec.Decode(&p); err != nil {
		// An empty document decodes to io.EOF, which is a legitimate profile
		// that inherits everything.
		if errors.Is(err, fmt.Errorf("EOF")) || err.Error() == "EOF" {
			return Profile{}, nil
		}
		return Profile{}, err
	}
	return p, nil
}

// Merge layers child over parent, returning the result.
//
// Scalars override when non-empty. Maps merge key by key, so a child adding one
// display name keeps the parent's others. Slices *replace* wholesale rather than
// appending: appending would make it impossible for a child to narrow an
// inherited list, and a profile that cannot remove its parent's namespaces is
// not really an override. To extend a parent's list, restate it.
func Merge(parent, child Profile) Profile {
	out := parent

	// Identity always comes from the child; it is the profile being loaded.
	out.Name = child.Name
	out.Extends = child.Extends
	if child.Description != "" {
		out.Description = child.Description
	}

	out.Clusters.Management = mergeClusterRef(parent.Clusters.Management, child.Clusters.Management)
	out.Clusters.Workload = mergeClusterRef(parent.Clusters.Workload, child.Clusters.Workload)

	if len(child.NodeRoles.LabelKeys) > 0 {
		out.NodeRoles.LabelKeys = child.NodeRoles.LabelKeys
	}
	if len(child.NodeRoles.CordonExpected) > 0 {
		out.NodeRoles.CordonExpected = child.NodeRoles.CordonExpected
	}
	out.NodeRoles.Display = mergeStringMap(parent.NodeRoles.Display, child.NodeRoles.Display)
	out.NodeRoles.MachineDeploymentMatch = mergeStringSliceMap(
		parent.NodeRoles.MachineDeploymentMatch, child.NodeRoles.MachineDeploymentMatch)

	if len(child.Events.Namespaces) > 0 {
		out.Events.Namespaces = child.Events.Namespaces
	}
	if len(child.Events.NamespacePrefixes) > 0 {
		out.Events.NamespacePrefixes = child.Events.NamespacePrefixes
	}
	if child.Events.AllNamespaces {
		out.Events.AllNamespaces = true
	}

	if len(child.CriticalWorkloads) > 0 {
		out.CriticalWorkloads = child.CriticalWorkloads
	}

	out.Plugins = mergeSettings(parent.Plugins, child.Plugins)

	if len(child.Layout.TopRow) > 0 {
		out.Layout.TopRow = child.Layout.TopRow
	}
	if len(child.Layout.Grid) > 0 {
		out.Layout.Grid = child.Layout.Grid
	}
	out.Layout.Stack = mergeStackMap(parent.Layout.Stack, child.Layout.Stack)

	return out
}

func mergeClusterRef(parent, child ClusterRef) ClusterRef {
	out := parent
	if child.Context != "" {
		out.Context = child.Context
	}
	if child.ContextPattern != "" {
		out.ContextPattern = child.ContextPattern
	}
	if child.CAPIName != "" {
		out.CAPIName = child.CAPIName
	}
	if child.CAPINamePattern != "" {
		out.CAPINamePattern = child.CAPINamePattern
	}
	if len(child.Namespaces) > 0 {
		out.Namespaces = child.Namespaces
	}
	return out
}

func mergeStringMap(parent, child map[string]string) map[string]string {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string]string, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

func mergeStringSliceMap(parent, child map[string][]string) map[string][]string {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string][]string, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

func mergeSettings(parent, child map[string]Settings) map[string]Settings {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string]Settings, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	// A plugin's settings block merges key by key, so a child overriding one
	// setting keeps the rest of the parent's block.
	for k, v := range child {
		if existing, ok := out[k]; ok {
			merged := make(Settings, len(existing)+len(v))
			for ek, ev := range existing {
				merged[ek] = ev
			}
			for ck, cv := range v {
				merged[ck] = cv
			}
			out[k] = merged
			continue
		}
		out[k] = v
	}
	return out
}

func mergeStackMap(parent, child map[string]StackSpec) map[string]StackSpec {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string]StackSpec, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

// Validate reports every problem with a profile at once.
//
// Problems are collected rather than returned one at a time: fixing a config
// file one error per run is a poor experience, and the checks are independent.
func (p Profile) Validate() error {
	var problems []string

	if p.Name == "" {
		problems = append(problems, "name is empty")
	}

	for role, patterns := range p.NodeRoles.MachineDeploymentMatch {
		for _, pat := range patterns {
			if pat == "" {
				problems = append(problems,
					fmt.Sprintf("node_roles.machinedeployment_match[%q] contains an empty pattern, which would match every name", role))
			}
		}
	}

	for _, key := range p.NodeRoles.LabelKeys {
		switch key {
		case "":
			problems = append(problems, "node_roles.label_keys contains an empty key")
		case WildcardSuffix:
			problems = append(problems,
				fmt.Sprintf("node_roles.label_keys contains a bare %q, which has no prefix to match", WildcardSuffix))
		}
	}

	for _, ns := range p.Events.NamespacePrefixes {
		if ns == "" {
			problems = append(problems,
				"events.namespace_prefixes contains an empty prefix, which would match every namespace")
		}
	}

	for i, c := range p.CriticalWorkloads {
		if c.Name == "" {
			problems = append(problems, fmt.Sprintf("critical_workloads[%d] has no name", i))
		}
		if c.Namespace == "" {
			problems = append(problems,
				fmt.Sprintf("critical_workloads[%d] (%q) has no namespace", i, c.Name))
		}
	}

	problems = append(problems, validatePattern("clusters.management.context_pattern", p.Clusters.Management.ContextPattern)...)
	problems = append(problems, validatePattern("clusters.workload.context_pattern", p.Clusters.Workload.ContextPattern)...)
	problems = append(problems, validatePattern("clusters.management.capi_name_pattern", p.Clusters.Management.CAPINamePattern)...)
	problems = append(problems, validatePattern("clusters.workload.capi_name_pattern", p.Clusters.Workload.CAPINamePattern)...)
	problems = append(problems, p.validateLayout()...)

	if len(problems) > 0 {
		sort.Strings(problems)
		return &ValidationError{Profile: p.Name, Problems: problems}
	}
	return nil
}

func validatePattern(field, pattern string) []string {
	if pattern == "" {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return []string{fmt.Sprintf("%s is not a valid regular expression: %v", field, err)}
	}
	return nil
}

// validateLayout checks pane references against each other.
//
// A pane ID cannot be checked against the real catalog here, because which panes
// exist depends on plugin detection against a live cluster. What can be checked
// is internal consistency: a stacked pane must name a host that the layout also
// places, and a ratio outside (0,1) is meaningless.
func (p Profile) validateLayout() []string {
	var problems []string

	placed := map[string]bool{}
	for _, id := range p.Layout.TopRow {
		if placed[id] {
			problems = append(problems, fmt.Sprintf("layout: pane %q appears twice", id))
		}
		placed[id] = true
	}
	for _, id := range p.Layout.Grid {
		if placed[id] {
			problems = append(problems, fmt.Sprintf("layout: pane %q appears twice", id))
		}
		placed[id] = true
	}

	for pane, spec := range p.Layout.Stack {
		switch {
		case spec.Under == "":
			problems = append(problems, fmt.Sprintf("layout.stack[%q] has no host pane", pane))
		case spec.Under == pane:
			problems = append(problems, fmt.Sprintf("layout.stack[%q] is stacked under itself", pane))
		case !placed[spec.Under]:
			problems = append(problems, fmt.Sprintf(
				"layout.stack[%q] names host %q, which the layout does not place", pane, spec.Under))
		}
		if spec.Ratio <= 0 || spec.Ratio >= 1 {
			problems = append(problems, fmt.Sprintf(
				"layout.stack[%q] ratio %v is outside the range (0, 1)", pane, spec.Ratio))
		}
	}
	return problems
}
