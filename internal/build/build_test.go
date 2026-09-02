package build

import (
	"runtime"
	"strings"
	"testing"
)

func TestInfoShort(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// goreleaser stamps the tag with its v stripped, and the header puts it
		// back, because the release is called v1.4.0 everywhere else.
		{"release tag", "1.4.0", "v1.4.0"},
		{"prerelease", "2.0.0-rc.1", "v2.0.0-rc.1"},
		{"already prefixed", "v1.4.0", "v1.4.0"},
		// A source build is not a tag and must not be made to look like one.
		{"source build", "dev", "dev"},
		{"padded", "  1.4.0  ", "v1.4.0"},
		// Nothing to report reports nothing, rather than captioning the tool's
		// name with an empty string or a guess.
		{"unset", "", ""},
		{"blank", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Info{Version: tc.in}).Short(); got != tc.want {
				t.Errorf("Info{Version: %q}.Short() = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInfoLine(t *testing.T) {
	line := Info{Version: "1.4.0", Commit: "abc1234", Date: "2026-08-25T00:00:00Z"}.Line()
	for _, want := range []string{
		"sextant", "1.4.0", "abc1234", "2026-08-25",
		runtime.GOOS, runtime.GOARCH, runtime.Version(),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("--version line should carry %q: %q", want, line)
		}
	}
	// The v is left off here: the flag is reporting the stamp it was built with,
	// not decorating it, and this string is what people paste into issues.
	if strings.Contains(line, "v1.4.0") {
		t.Errorf("Line should report the raw stamp: %q", line)
	}
}

// A build with no metadata still has to produce a readable line, since an
// unstamped binary is exactly the case where somebody is asking what it is.
func TestInfoLine_ZeroValue(t *testing.T) {
	line := Info{}.Line()
	if strings.Count(line, "unknown") != 3 {
		t.Errorf("every missing field should read unknown: %q", line)
	}
}
