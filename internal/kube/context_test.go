package kube

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func entries() []ContextEntry {
	return []ContextEntry{
		{Name: "kind-capi-management", Cluster: "kind-capi-management", AuthInfo: "kind-capi-management", Current: true},
		{Name: "prod-mgmt", Cluster: "prod", AuthInfo: "admin"},
		{Name: "prod-workload-a", Cluster: "prod-a", AuthInfo: "admin"},
		{Name: "prod-workload-b", Cluster: "prod-b", AuthInfo: "admin"},
	}
}

// --- the zero-config path -------------------------------------------------

// This is the case that has to work with no configuration whatsoever: a user
// whose kubectl already points at a management cluster.
func TestResolve_ZeroSelectorUsesCurrentContext(t *testing.T) {
	got, err := Resolve(entries(), RoleManagement, Selector{}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "kind-capi-management" {
		t.Errorf("got %q want the current context", got.Name)
	}
}

func TestResolve_ZeroSelectorWithNoCurrentContext(t *testing.T) {
	es := entries()
	es[0].Current = false
	_, err := Resolve(es, RoleManagement, Selector{}, nil)

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("got %v want NotFoundError", err)
	}
	// The message should tell the user what they do have.
	if !strings.Contains(nf.Error(), "prod-mgmt") {
		t.Errorf("error should list available contexts: %v", nf)
	}
	if !strings.Contains(nf.Error(), "current context") {
		t.Errorf("error should say what was looked for: %v", nf)
	}
}

func TestSelectorIsZero(t *testing.T) {
	if !(Selector{}).IsZero() {
		t.Error("empty selector should be zero")
	}
	if (Selector{Context: "x"}).IsZero() || (Selector{Pattern: "x"}).IsZero() {
		t.Error("a populated selector should not be zero")
	}
}

// --- explicit context -----------------------------------------------------

func TestResolve_ExplicitContextWins(t *testing.T) {
	// Explicit name beats the current context.
	got, err := Resolve(entries(), RoleManagement, Selector{Context: "prod-mgmt"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "prod-mgmt" {
		t.Errorf("got %q want prod-mgmt", got.Name)
	}
}

func TestResolve_ExplicitContextBeatsPattern(t *testing.T) {
	sel := Selector{Context: "prod-mgmt", Pattern: "workload"}
	got, err := Resolve(entries(), RoleManagement, sel, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "prod-mgmt" {
		t.Errorf("got %q want prod-mgmt (explicit context outranks pattern)", got.Name)
	}
}

func TestResolve_UnknownExplicitContext(t *testing.T) {
	_, err := Resolve(entries(), RoleWorkload, Selector{Context: "nope"}, nil)
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("got %v want NotFoundError", err)
	}
	if !strings.Contains(nf.Error(), `"nope"`) {
		t.Errorf("error should name the missing context: %v", nf)
	}
}

// --- patterns -------------------------------------------------------------

func TestResolve_UnambiguousPattern(t *testing.T) {
	got, err := Resolve(entries(), RoleManagement, Selector{Pattern: "-mgmt$"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "prod-mgmt" {
		t.Errorf("got %q want prod-mgmt", got.Name)
	}
}

func TestResolve_AmbiguousPatternWithoutPicker(t *testing.T) {
	_, err := Resolve(entries(), RoleWorkload, Selector{Pattern: "workload"}, nil)
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("got %v want AmbiguousError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("candidates: got %d want 2", len(amb.Candidates))
	}
	// The message should be actionable.
	if !strings.Contains(amb.Error(), "--workload-context") {
		t.Errorf("error should suggest pinning a context: %v", amb)
	}
}

func TestResolve_AmbiguousPatternWithPicker(t *testing.T) {
	var sawRole Role
	var sawCount int
	picker := func(role Role, candidates []Candidate) (int, error) {
		sawRole, sawCount = role, len(candidates)
		return 1, nil
	}
	got, err := Resolve(entries(), RoleWorkload, Selector{Pattern: "workload"}, picker)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Candidates are name-sorted, so index 1 is workload-b.
	if got.Name != "prod-workload-b" {
		t.Errorf("got %q want prod-workload-b", got.Name)
	}
	if sawRole != RoleWorkload || sawCount != 2 {
		t.Errorf("picker got role=%q count=%d", sawRole, sawCount)
	}
}

// An unambiguous match must never prompt — the happy path stays silent.
func TestResolve_UnambiguousDoesNotConsultPicker(t *testing.T) {
	picker := func(Role, []Candidate) (int, error) {
		t.Fatal("picker consulted for an unambiguous resolution")
		return 0, nil
	}
	if _, err := Resolve(entries(), RoleManagement, Selector{Pattern: "-mgmt$"}, picker); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := Resolve(entries(), RoleManagement, Selector{}, picker); err != nil {
		t.Fatalf("Resolve with zero selector: %v", err)
	}
}

func TestResolve_PickerError(t *testing.T) {
	boom := errors.New("user canceled")
	picker := func(Role, []Candidate) (int, error) { return 0, boom }
	_, err := Resolve(entries(), RoleWorkload, Selector{Pattern: "workload"}, picker)
	if !errors.Is(err, boom) {
		t.Errorf("got %v want %v", err, boom)
	}
}

func TestResolve_PickerOutOfRange(t *testing.T) {
	for _, idx := range []int{-1, 2, 99} {
		picker := func(Role, []Candidate) (int, error) { return idx, nil }
		if _, err := Resolve(entries(), RoleWorkload, Selector{Pattern: "workload"}, picker); err == nil {
			t.Errorf("index %d: expected an error", idx)
		}
	}
}

func TestResolve_InvalidPattern(t *testing.T) {
	_, err := Resolve(entries(), RoleManagement, Selector{Pattern: "([unclosed"}, nil)
	var bad *InvalidPatternError
	if !errors.As(err, &bad) {
		t.Fatalf("got %v want InvalidPatternError", err)
	}
	if bad.Unwrap() == nil {
		t.Error("InvalidPatternError should wrap the compile error")
	}
}

// --- matching surface -----------------------------------------------------

// A pattern is tried against name, cluster and user, since conventions differ
// about which of them carries cluster identity.
func TestContextEntryMatches_AllThreeFields(t *testing.T) {
	e := ContextEntry{Name: "ctx", Cluster: "the-cluster", AuthInfo: "the-user"}
	for _, pat := range []string{"^ctx$", "the-cluster", "the-user"} {
		if !e.Matches(regexp.MustCompile(pat)) {
			t.Errorf("pattern %q should match", pat)
		}
	}
	if e.Matches(regexp.MustCompile("absent")) {
		t.Error("unrelated pattern should not match")
	}
}

func TestFind_SortsByName(t *testing.T) {
	shuffled := []ContextEntry{
		{Name: "z-workload"}, {Name: "a-workload"}, {Name: "m-workload"},
	}
	got, err := Find(shuffled, Selector{Pattern: "workload"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	var order []string
	for _, c := range got {
		order = append(order, c.Entry.Name)
	}
	if strings.Join(order, ",") != "a-workload,m-workload,z-workload" {
		t.Errorf("got %v want name-sorted", order)
	}
}

// Why is surfaced in diagnostics, so each path should populate it.
func TestFind_RecordsWhy(t *testing.T) {
	for _, tc := range []struct {
		sel  Selector
		want string
	}{
		{Selector{Context: "prod-mgmt"}, "explicit"},
		{Selector{Pattern: "-mgmt$"}, "pattern"},
		{Selector{}, "current"},
	} {
		got, err := Find(entries(), tc.sel)
		if err != nil {
			t.Fatalf("Find(%+v): %v", tc.sel, err)
		}
		if len(got) == 0 {
			t.Fatalf("Find(%+v): no candidates", tc.sel)
		}
		if !strings.Contains(got[0].Why, tc.want) {
			t.Errorf("Find(%+v).Why = %q, want it to mention %q", tc.sel, got[0].Why, tc.want)
		}
	}
}

func TestFind_EmptyEntries(t *testing.T) {
	for _, sel := range []Selector{{}, {Context: "x"}, {Pattern: "x"}} {
		got, err := Find(nil, sel)
		if err != nil {
			t.Errorf("Find(nil, %+v): %v", sel, err)
		}
		if len(got) != 0 {
			t.Errorf("Find(nil, %+v): got %v want none", sel, got)
		}
	}
}

func TestNotFoundError_NoContextsAtAll(t *testing.T) {
	_, err := Resolve(nil, RoleManagement, Selector{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no contexts") {
		t.Errorf("got %v want a message about an empty kubeconfig", err)
	}
}
