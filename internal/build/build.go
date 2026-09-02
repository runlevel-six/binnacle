// Package build carries the version metadata stamped into the binary at link
// time, so that the dashboard can name the build it is.
//
// The linker still writes into cmd/sextant's own variables, and main hands them
// here; the -X flags do not point at this package. That is deliberate: the
// ldflags in the Makefile and .goreleaser.yaml name main.version, and a symbol
// that has moved does not fail a build — it silently leaves every release
// reporting itself as a source build.
package build

import (
	"fmt"
	"runtime"
	"strings"
)

// Info describes the running binary.
type Info struct {
	// Version is a release tag with its leading "v" already stripped
	// ("1.4.0"), or "dev" for a build straight from source.
	Version string
	// Commit is the revision the build came from. Release builds stamp the full
	// SHA, which is why nothing puts it on screen beside [Info.Short].
	Commit string
	// Date is the commit date, in RFC 3339.
	Date string
}

// Short is the version as the header shows it: a release tag gets its "v" back,
// and any other stamp is left exactly as it is.
//
// goreleaser strips the leading v from the tag it injects, but "v1.4.0" is how
// that release is named everywhere a user will go looking — the git tag, the
// archive, the release page. A bare "1.4.0" beside the tool's name reads like a
// number that could be anything. "dev" is not a tag and must not be dressed up
// as one, so it, and any other non-numeric stamp, passes through untouched.
//
// An unset version returns the empty string rather than a guess, which is what
// keeps the header from captioning the name with nothing.
func (i Info) Short() string {
	v := strings.TrimSpace(i.Version)
	if v == "" {
		return ""
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// Line is the one-line report --version prints.
//
// It names the toolchain and platform as well as the build, because the first
// thing a bug report needs to establish is which binary was running.
func (i Info) Line() string {
	return fmt.Sprintf("sextant %s (commit %s, built %s, %s/%s, %s)",
		orUnknown(i.Version), orUnknown(i.Commit), orUnknown(i.Date),
		runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// orUnknown keeps a missing field from rendering as a gap in the middle of a
// line someone is about to paste into an issue.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
