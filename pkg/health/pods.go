package health

import (
	"strings"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
)

// PodGrace is how long a pod may be starting before it counts against a
// cluster's health.
//
// A minute is long enough to pull an image on a warm node and short enough that
// a pod genuinely stuck pulling one still shows up while somebody is watching.
const PodGrace = time.Minute

// NeedsAttention reports whether an unhealthy pod is worth reporting.
//
// [model.Pod.IsHealthy] answers a narrower question — is every container ready
// right now — and a pod one second into being created answers no. On a cluster
// running anything that creates pods continuously, a vulnerability scanner or a
// backup CronJob, that is a permanent supply of pods that are not ready and
// never were a problem: the count never reaches zero, the health cell never
// goes green, and the number flickers between whatever two scans happened to be
// starting. A signal that is never clear is a signal nobody reads.
//
// So a pod that is merely starting, has not restarted, and is younger than
// [PodGrace] is not counted. Everything else is, including a pod that has been
// starting for an hour: the age is what separates a slow scheduler from a
// broken one.
//
// This is a judgement rather than a projection, which is why it lives here and
// not on the type: both front ends have to make it the same way, or the fleet
// page and the dashboard disagree about whether a cluster is healthy and there
// is no way to tell which is right.
func NeedsAttention(p model.Pod) bool {
	return !p.IsHealthy && !starting(p)
}

// starting reports whether a pod is early in its own creation.
//
// Age comes from the projection, which re-runs on every publish, so it is the
// age as of the last snapshot rather than of this instant. That is close enough
// for a minute-long grace period and it keeps the verdict a pure function of
// the model.
func starting(p model.Pod) bool {
	// A restart means the pod already ran and stopped, whatever it is doing now.
	if p.Age >= PodGrace || p.Restarts > 0 {
		return false
	}
	switch p.Status {
	case "ContainerCreating", "PodInitializing", "Pending":
		return true
	}
	// "Init:2/3" is an init container that has not finished yet. "Init:Error"
	// and "Init:CrashLoopBackOff" are ones that failed, and both wear the same
	// prefix — the digit is what separates a pod starting from a pod broken
	// during startup, which no grace period should excuse.
	if rest, ok := strings.CutPrefix(p.Status, "Init:"); ok && rest != "" {
		return rest[0] >= '0' && rest[0] <= '9'
	}
	return false
}
