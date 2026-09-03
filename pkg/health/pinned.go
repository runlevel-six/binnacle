package health

import (
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
)

// Pin is one profile-declared workload and how much of it is running.
//
// It answers a question an unhealthy-pod list structurally cannot: a workload
// that is *gone* has no unhealthy pods, so a dashboard built only from what is
// failing reports a deleted database and a scaled-to-zero controller as
// silence. Pinning the name is what turns absence into a row.
//
// Ready and Desired count pods, not replicas as the controller declares them —
// this is derived from a pod snapshot, so a workload nobody has scheduled reads
// 0/0 and Absent, which is the state [Pin.State] is careful to separate from
// healthy.
type Pin struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ready     int    `json:"ready"`
	Desired   int    `json:"desired"`
	// Absent means no pod matched at all. It is a field rather than a
	// `Desired == 0` check at each call site because the difference between
	// "none running" and "none wanted" is the whole point of pinning a name,
	// and a caller that has to remember to derive it is a caller that will
	// eventually forget.
	Absent bool `json:"absent"`
}

// State names the verdict: "missing", "degraded" or "healthy".
//
// Stable and lowercase, so a template can use it as a CSS class and a terminal
// can print it. Nothing here decides a color; [Pin.Status] does the folding a
// caller needs for that.
func (p Pin) State() string {
	switch {
	case p.Absent:
		return "missing"
	case p.Ready < p.Desired:
		return "degraded"
	}
	return "healthy"
}

// Status is the severity of State, for a caller folding pins into an indicator.
//
// Absent is an error rather than a warning. A pinned workload was named because
// somebody decided its absence is not survivable, and reporting that in the
// same color as a restarting pod loses the distinction the pin was made for.
func (p Pin) Status() Status {
	switch {
	case p.Absent:
		return StatusErr
	case p.Ready < p.Desired:
		return StatusWarn
	}
	return StatusOK
}

// Pins matches each declared workload against a pod snapshot, in the order the
// profile declares them.
//
// The order is the profile's on purpose: a site lists its workloads in the
// order it thinks about them, and re-sorting by severity would move rows around
// under a reader between refreshes. Ranking is for lists whose length the
// cluster decides; this one is as long as the profile.
//
// It returns nil for an empty list, so a caller can use the result's emptiness
// to decide whether the section exists at all — a profile that pins nothing
// should render no table rather than an empty one.
func Pins(pods []model.Pod, declared []profile.CriticalWorkload) []Pin {
	if len(declared) == 0 {
		return nil
	}
	out := make([]Pin, 0, len(declared))
	for _, w := range declared {
		pin := Pin{Kind: w.Kind, Namespace: w.Namespace, Name: w.Name}
		for _, p := range pods {
			if w.Matches(p.Namespace, p.Name) {
				pin.Desired++
				if p.IsHealthy {
					pin.Ready++
				}
			}
		}
		pin.Absent = pin.Desired == 0
		out = append(out, pin)
	}
	return out
}
