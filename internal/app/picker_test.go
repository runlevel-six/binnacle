package app

import (
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/internal/kube"
)

func candidates(names ...string) []kube.Candidate {
	out := make([]kube.Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, kube.Candidate{Entry: kube.ContextEntry{Name: n}})
	}
	return out
}

var siteContexts = candidates(
	"site-b-mgmt-01",
	"site-c-mgmt-01",
	"site-a-mgmt-01",
)

func TestTerminalPickerSelectsByIndex(t *testing.T) {
	var out strings.Builder
	pick := TerminalPicker(strings.NewReader("2\n"), &out)

	idx, err := pick(kube.RoleManagement, siteContexts)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != 1 {
		t.Errorf("index = %d, want 1", idx)
	}
	// The prompt must list every candidate; choosing blind is not choosing.
	for _, c := range siteContexts {
		if !strings.Contains(out.String(), c.Entry.Name) {
			t.Errorf("prompt does not mention %s:\n%s", c.Entry.Name, out.String())
		}
	}
}

func TestTerminalPickerSelectsBySubstring(t *testing.T) {
	// The datacentre is the part the operator is actually choosing between.
	pick := TerminalPicker(strings.NewReader("site-a\n"), &strings.Builder{})

	idx, err := pick(kube.RoleManagement, siteContexts)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got := siteContexts[idx].Entry.Name; got != "site-a-mgmt-01" {
		t.Errorf("chose %s, want site-a-mgmt-01", got)
	}
}

func TestTerminalPickerSelectsByExactName(t *testing.T) {
	pick := TerminalPicker(strings.NewReader("site-c-mgmt-01\n"), &strings.Builder{})

	idx, err := pick(kube.RoleWorkload, siteContexts)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != 1 {
		t.Errorf("index = %d, want 1", idx)
	}
}

// A candidate literally named "2" must not shadow the row numbering the prompt
// just printed, or the numbers on screen would mean two different things.
func TestTerminalPickerPrefersIndexOverName(t *testing.T) {
	list := candidates("2", "other")
	pick := TerminalPicker(strings.NewReader("2\n"), &strings.Builder{})

	idx, err := pick(kube.RoleManagement, list)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != 1 {
		t.Errorf("index = %d, want 1 (the second row), not the context named \"2\"", idx)
	}
}

func TestTerminalPickerRepromptsThenSucceeds(t *testing.T) {
	var out strings.Builder
	// Out of range, then ambiguous, then valid.
	pick := TerminalPicker(strings.NewReader("9\nmgmt\n1\n"), &out)

	idx, err := pick(kube.RoleManagement, siteContexts)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != 0 {
		t.Errorf("index = %d, want 0", idx)
	}
	if !strings.Contains(out.String(), "no option 9") {
		t.Errorf("out-of-range answer not explained:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "matches 3 of them") {
		t.Errorf("ambiguous answer not explained:\n%s", out.String())
	}
}

func TestTerminalPickerGivesUpAfterThreeBadAnswers(t *testing.T) {
	pick := TerminalPicker(strings.NewReader("x\ny\nz\n1\n"), &strings.Builder{})

	if _, err := pick(kube.RoleManagement, siteContexts); err == nil {
		t.Fatal("want an error after repeated bad answers, got nil")
	} else if !strings.Contains(err.Error(), "--management-context") {
		t.Errorf("error should name the flag that settles it, got: %v", err)
	}
}

func TestTerminalPickerQuitNamesTheFlag(t *testing.T) {
	for _, answer := range []string{"q\n", "quit\n", "\n", ""} {
		pick := TerminalPicker(strings.NewReader(answer), &strings.Builder{})

		_, err := pick(kube.RoleWorkload, siteContexts)
		if err == nil {
			t.Fatalf("answer %q: want an error, got nil", answer)
		}
		// The operator who declines has just learned the pattern is ambiguous;
		// the useful next thing is the flag, and the candidates to give it.
		if !strings.Contains(err.Error(), "--workload-context") {
			t.Errorf("answer %q: error should name the flag, got: %v", answer, err)
		}
		if !strings.Contains(err.Error(), "site-a-mgmt-01") {
			t.Errorf("answer %q: error should repeat the candidates, got: %v", answer, err)
		}
	}
}

// The server URL is what distinguishes contexts whose names differ only in a
// substring the operator may not recognize.
func TestTerminalPickerShowsServerAndCurrentMarker(t *testing.T) {
	list := []kube.Candidate{
		{Entry: kube.ContextEntry{Name: "a", Server: "https://a.example:6443"}},
		{Entry: kube.ContextEntry{Name: "b", Server: "https://b.example:6443", Current: true}},
	}
	var out strings.Builder
	pick := TerminalPicker(strings.NewReader("1\n"), &out)

	if _, err := pick(kube.RoleManagement, list); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if !strings.Contains(out.String(), "https://b.example:6443") {
		t.Errorf("prompt omits the server URL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "*") || !strings.Contains(out.String(), "current kubeconfig context") {
		t.Errorf("prompt does not mark the current context:\n%s", out.String())
	}
}

// Resolve wiring: a picker is only consulted when a pattern is genuinely
// ambiguous, so the common path never prompts. Guard that here, since a
// dashboard that stops to ask a question on every start would be unusable.
func TestPickerNotConsultedWhenUnambiguous(t *testing.T) {
	entries := []kube.ContextEntry{
		{Name: "site-a-mgmt-01"},
		{Name: "unrelated"},
	}
	consulted := false
	picker := func(kube.Role, []kube.Candidate) (int, error) {
		consulted = true
		return 0, nil
	}

	got, err := kube.Resolve(entries, kube.RoleManagement,
		kube.Selector{Pattern: `-mgmt-\d+$`}, picker)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if consulted {
		t.Error("picker consulted for an unambiguous pattern")
	}
	if got.Name != "site-a-mgmt-01" {
		t.Errorf("resolved %s", got.Name)
	}
}
