package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/runlevel-six/binnacle/internal/kube"
)

// promptAttempts is how many times a malformed answer is re-asked before giving
// up. Enough to recover from a typo, few enough that a script piping the wrong
// thing into stdin fails quickly rather than looping.
const promptAttempts = 3

// InteractivePicker returns a picker that prompts on the terminal, or nil when
// there is no terminal to prompt on.
//
// Nil is the important half of this. Without a picker an ambiguous pattern
// becomes a [kube.AmbiguousError], which names the candidates and the flag that
// pins one — exactly what a script, a CI job or a piped invocation needs, since
// none of them can answer a question. Prompting is reserved for the case where
// someone is actually watching.
//
// Both streams are checked: stdin because the answer is read from it, and stderr
// because the question is written there. A run with either end redirected gets
// the error, not a prompt written somewhere nobody will see.
func InteractivePicker() kube.Picker {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil
	}
	return TerminalPicker(os.Stdin, os.Stderr)
}

// TerminalPicker builds a picker that writes its question to out and reads the
// answer from in.
//
// The prompt goes to stderr rather than stdout in the real invocation, so that
// `--dry-run` output stays pipeable while the question still reaches the
// operator's screen.
//
// An answer may be an index or any substring that identifies exactly one
// candidate. Substring matching is not a flourish: real context names are long
// and differ in a single segment — site-b-mgmt-01 against site-a-mgmt-01 — and
// that segment, the datacentre, is the part the operator is actually choosing
// between, so typing "site-a" is both shorter and less error-prone than
// counting rows.
func TerminalPicker(in io.Reader, out io.Writer) kube.Picker {
	reader := bufio.NewReader(in)

	return func(role kube.Role, candidates []kube.Candidate) (int, error) {
		fmt.Fprintf(out, "\nSeveral contexts match the %s cluster:\n\n", role)
		for i, c := range candidates {
			marker := " "
			if c.Entry.Current {
				marker = "*"
			}
			fmt.Fprintf(out, " %s %d) %s\n", marker, i+1, c.Entry.Name)
			if c.Entry.Server != "" {
				// The server disambiguates contexts whose names differ only in a
				// substring the operator may not recognize.
				fmt.Fprintf(out, "        %s\n", c.Entry.Server)
			}
		}
		if anyCurrent(candidates) {
			fmt.Fprintln(out, "\n * marks your current kubeconfig context.")
		}

		for attempt := range promptAttempts {
			fmt.Fprintf(out, "\nSelect [1-%d], a name, or q to quit: ", len(candidates))

			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				// EOF with nothing typed: the terminal went away mid-prompt, or
				// input was closed. Fall back to the message that names the flag.
				return 0, abort(role, candidates)
			}

			answer := strings.TrimSpace(line)
			switch {
			case answer == "", strings.EqualFold(answer, "q"), strings.EqualFold(answer, "quit"):
				return 0, abort(role, candidates)
			}

			idx, err := match(answer, candidates)
			if err == nil {
				return idx, nil
			}
			fmt.Fprintf(out, "  %v\n", err)

			if attempt == promptAttempts-1 {
				return 0, abort(role, candidates)
			}
		}
		return 0, abort(role, candidates)
	}
}

// match resolves an answer to a candidate index.
//
// A number is tried first, so a candidate that happens to be named "2" cannot
// shadow the row numbering the prompt just printed. Then an exact name, then a
// unique case-insensitive substring.
func match(answer string, candidates []kube.Candidate) (int, error) {
	if n, err := strconv.Atoi(answer); err == nil {
		if n < 1 || n > len(candidates) {
			return 0, fmt.Errorf("no option %d; choose between 1 and %d", n, len(candidates))
		}
		return n - 1, nil
	}

	for i, c := range candidates {
		if c.Entry.Name == answer {
			return i, nil
		}
	}

	var found []int
	lower := strings.ToLower(answer)
	for i, c := range candidates {
		if strings.Contains(strings.ToLower(c.Entry.Name), lower) {
			found = append(found, i)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return 0, fmt.Errorf("%q matches none of them", answer)
	default:
		names := make([]string, 0, len(found))
		for _, i := range found {
			names = append(names, candidates[i].Entry.Name)
		}
		return 0, fmt.Errorf("%q matches %d of them (%s); be more specific",
			answer, len(found), strings.Join(names, ", "))
	}
}

// abort reports a declined prompt.
//
// It names the flag, the way [kube.AmbiguousError] does for an unattended run:
// someone who quits the prompt has just learned that their pattern is ambiguous,
// and the next thing they need is how to settle it permanently. The candidates
// are repeated because the prompt may have scrolled off by the time the shell
// prints this.
func abort(role kube.Role, candidates []kube.Candidate) error {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.Entry.Name)
	}
	return fmt.Errorf("no %s cluster chosen from %d matching contexts (%s); pin one with --%s-context",
		role, len(candidates), strings.Join(names, ", "), role)
}

func anyCurrent(candidates []kube.Candidate) bool {
	for _, c := range candidates {
		if c.Entry.Current {
			return true
		}
	}
	return false
}
