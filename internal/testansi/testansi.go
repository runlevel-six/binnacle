// Package testansi inspects rendered frames in tests.
//
// It exists because the same two questions get asked of a frame from two
// packages that cannot import each other's tests: internal/ui renders a single
// pane, and internal/app renders a whole dashboard including the plugin panes
// that only exist once a registry has detected them. Both need to know whether
// every visible cell was painted.
//
// Nothing outside a test should import this.
package testansi

import (
	"regexp"
	"strings"
	"testing"
)

// Reset is the sequence lipgloss ends every styled span with. It clears the
// background as well as the foreground, which is the whole reason these checks
// are necessary.
const Reset = "\x1b[0m"

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Backgrounds returns the distinct background parameters a rendered line sets, in
// the order they first appear.
//
// Callers are expected to have selected the TrueColor profile, where a background
// reads as a "48;" parameter.
func Backgrounds(line string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seq := range sgr.FindAllString(line, -1) {
		idx := strings.Index(seq, "48;")
		if idx < 0 {
			continue
		}
		if bg := strings.TrimSuffix(seq[idx:], "m"); !seen[bg] {
			seen[bg] = true
			out = append(out, bg)
		}
	}
	return out
}

// AssertNoHoles fails if any visible text in line is drawn while no background is
// armed.
//
// It tracks the state a terminal would: a reset disarms the background, and any
// sequence carrying a background parameter arms it. An unpainted run inside a
// grounded theme is a hole in the panel — the failure mode that a nested style's
// reset creates and that lipgloss does not re-arm on its own.
func AssertNoHoles(t testing.TB, label, line string) {
	t.Helper()
	armed := false
	for len(line) > 0 {
		if loc := sgr.FindStringIndex(line); loc != nil && loc[0] == 0 {
			seq := line[:loc[1]]
			switch {
			case seq == Reset:
				armed = false
			case strings.Contains(seq, "48;"):
				armed = true
			}
			line = line[loc[1]:]
			continue
		}
		end := strings.Index(line, "\x1b")
		if end < 0 {
			end = len(line)
		}
		if !armed {
			t.Errorf("%s: unpainted run %q", label, line[:end])
			return
		}
		line = line[end:]
	}
}

// StripANSI removes every escape sequence, leaving the visible text.
func StripANSI(s string) string {
	var sb strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
