// Package kube loads kubeconfig data and resolves which context to use for
// each cluster.
//
// The resolution order is what makes the zero-config path work:
//
//  1. An explicitly named context, from a flag or a profile.
//  2. A profile-supplied regular expression, for sites whose context names
//     follow a convention.
//  3. The kubeconfig's current context.
//
// Step 3 is the default and needs no configuration at all — pointing kubectl at
// a management cluster is enough to point sextant at it too. Step 2 exists
// because some sites do encode cluster identity in context names, but it is
// strictly opt-in: a pattern is never assumed.
//
// The resolver works on [ContextEntry] values rather than on client-go types so
// that it is testable without touching disk or a cluster.
package kube

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Role names which cluster a context is being resolved for.
//
// The vocabulary is upstream's: a *management* cluster runs the Cluster API
// controllers, and a *workload* cluster is one they provision.
type Role string

// Cluster roles.
const (
	RoleManagement Role = "management"
	RoleWorkload   Role = "workload"
)

// ContextEntry is the subset of a kubeconfig context the resolver needs. It
// mirrors what `kubectl config get-contexts` shows.
type ContextEntry struct {
	Name     string
	Cluster  string
	AuthInfo string
	// Server is the API server URL, shown in diagnostics to disambiguate
	// similarly named contexts.
	Server string
	// Current reports whether this is the kubeconfig's current context.
	Current bool
}

// Matches reports whether re matches any of the entry's identifying strings.
//
// All three are tried because conventions vary: some tools encode cluster
// identity in the context name, others only in the cluster or user name.
func (e ContextEntry) Matches(re *regexp.Regexp) bool {
	return re.MatchString(e.Name) || re.MatchString(e.Cluster) || re.MatchString(e.AuthInfo)
}

// Selector describes how to find one cluster's context.
//
// A zero Selector means "use the current context", which is the default path.
type Selector struct {
	// Context pins an exact context name. Highest precedence.
	Context string
	// Pattern is a regular expression over a context's name, cluster, and user.
	Pattern string
}

// IsZero reports whether the selector expresses no preference, in which case the
// current context is used.
func (s Selector) IsZero() bool { return s.Context == "" && s.Pattern == "" }

// Candidate is a context that matched a selector.
type Candidate struct {
	Entry ContextEntry
	// Why records how this candidate was found, for diagnostics.
	Why string
}

// NotFoundError reports that nothing matched.
type NotFoundError struct {
	Role     Role
	Selector Selector
	// Available lists the context names that were considered, so the message
	// can show the user what they actually have.
	Available []string
}

func (e *NotFoundError) Error() string {
	var what string
	switch {
	case e.Selector.Context != "":
		what = fmt.Sprintf("context %q", e.Selector.Context)
	case e.Selector.Pattern != "":
		what = fmt.Sprintf("pattern %q", e.Selector.Pattern)
	default:
		what = "the current context"
	}
	if len(e.Available) == 0 {
		return fmt.Sprintf("no %s cluster: %s not found, and the kubeconfig has no contexts", e.Role, what)
	}
	return fmt.Sprintf("no %s cluster: %s matched nothing (available: %s)",
		e.Role, what, strings.Join(e.Available, ", "))
}

// AmbiguousError reports that a pattern matched several contexts. It carries the
// candidates so a caller can prompt.
type AmbiguousError struct {
	Role       Role
	Selector   Selector
	Candidates []Candidate
}

func (e *AmbiguousError) Error() string {
	names := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		names[i] = c.Entry.Name
	}
	return fmt.Sprintf("pattern %q matches %d contexts for the %s cluster (%s); pin one with --%s-context",
		e.Selector.Pattern, len(e.Candidates), e.Role, strings.Join(names, ", "), e.Role)
}

// InvalidPatternError reports an uncompilable pattern.
type InvalidPatternError struct {
	Role    Role
	Pattern string
	Err     error
}

func (e *InvalidPatternError) Error() string {
	return fmt.Sprintf("invalid context pattern %q for the %s cluster: %v", e.Pattern, e.Role, e.Err)
}

func (e *InvalidPatternError) Unwrap() error { return e.Err }

// Picker disambiguates between candidates, returning the chosen index. A nil
// Picker turns ambiguity into an [AmbiguousError].
type Picker func(role Role, candidates []Candidate) (int, error)

// Find returns every context matching sel, sorted by name for stable ordering.
//
// An exact context name yields at most one candidate. A pattern may yield
// several. A zero selector yields the current context, if there is one.
func Find(entries []ContextEntry, sel Selector) ([]Candidate, error) {
	switch {
	case sel.Context != "":
		for _, e := range entries {
			if e.Name == sel.Context {
				return []Candidate{{Entry: e, Why: "named explicitly"}}, nil
			}
		}
		return nil, nil

	case sel.Pattern != "":
		re, err := regexp.Compile(sel.Pattern)
		if err != nil {
			return nil, err
		}
		var out []Candidate
		for _, e := range entries {
			if e.Matches(re) {
				out = append(out, Candidate{Entry: e, Why: "matched pattern " + sel.Pattern})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Entry.Name < out[j].Entry.Name })
		return out, nil

	default:
		for _, e := range entries {
			if e.Current {
				return []Candidate{{Entry: e, Why: "current context"}}, nil
			}
		}
		return nil, nil
	}
}

// Resolve picks exactly one context for role.
//
// The picker is consulted only when a pattern is genuinely ambiguous, so an
// unambiguous resolution never prompts. With a nil picker, ambiguity is
// returned as an [AmbiguousError] carrying the candidates.
func Resolve(entries []ContextEntry, role Role, sel Selector, picker Picker) (ContextEntry, error) {
	candidates, err := Find(entries, sel)
	if err != nil {
		return ContextEntry{}, &InvalidPatternError{Role: role, Pattern: sel.Pattern, Err: err}
	}

	switch len(candidates) {
	case 0:
		return ContextEntry{}, &NotFoundError{Role: role, Selector: sel, Available: names(entries)}
	case 1:
		return candidates[0].Entry, nil
	}

	if picker == nil {
		return ContextEntry{}, &AmbiguousError{Role: role, Selector: sel, Candidates: candidates}
	}
	idx, err := picker(role, candidates)
	if err != nil {
		return ContextEntry{}, err
	}
	if idx < 0 || idx >= len(candidates) {
		return ContextEntry{}, fmt.Errorf("context picker returned out-of-range index %d for %d candidates", idx, len(candidates))
	}
	return candidates[idx].Entry, nil
}

func names(entries []ContextEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}
