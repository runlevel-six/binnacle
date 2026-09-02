// Package kube holds the Kubernetes machinery every plugin needs: probes to
// detect whether a subsystem is present, and a helper to run a read-only command
// inside one of its pods.
//
// # Three tiers
//
// A plugin reports one of three [Tier] values, and the distinction is the whole
// point of the package. Full detail needs `pods/exec`; without that permission a
// plugin falls back to what informers can see; and if the subsystem is not
// installed the plugin is absent and contributes nothing.
//
// The tiers exist because the alternative — requiring exec, or erroring without
// it — makes the tool unusable for anyone whose RBAC is tighter than the author's.
// A thinner pane is a good outcome. An error wall is not.
package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/runlevel-six/binnacle/pkg/subsystem"
)

// Tier is how much detail a plugin can currently provide.
//
// An alias for [subsystem.Tier]: the vocabulary is public so that a consumer
// outside this module can read a plugin's state, while the machinery that
// derives it stays here.
type Tier = subsystem.Tier

// The tiers, in increasing detail. See [subsystem.Tier].
const (
	TierAbsent   = subsystem.TierAbsent
	TierInformer = subsystem.TierInformer
	TierFull     = subsystem.TierFull
)

// Client bundles what a plugin needs to probe and read a cluster.
//
// ExecConfig, when non-nil, is the identity used for pods/exec calls. This
// separates the read identity (typically a CAPI-minted kubeconfig) from the
// exec identity (a dedicated ServiceAccount with pods/exec scoped to the
// namespaces where Ceph, Cilium, and OVN pods run). When nil, exec falls back
// to Config — which is the historical behavior and what local sextant still
// does.
type Client struct {
	Typed      kubernetes.Interface
	Config     *rest.Config
	ExecConfig *rest.Config
	Mapper     meta.RESTMapper
}

// NewClient builds a Client from a REST config. Exec uses the same config.
func NewClient(cfg *rest.Config) (*Client, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	return &Client{
		Typed:  typed,
		Config: cfg,
		Mapper: restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscovery{disc}),
	}, nil
}

// NewClientWithExec builds a Client that reads with cfg but execs with
// execCfg. execCfg may be nil, in which case exec falls back to cfg — the
// same behavior as NewClient.
func NewClientWithExec(cfg, execCfg *rest.Config) (*Client, error) {
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	c.ExecConfig = execCfg
	return c, nil
}

// execConfig returns the config to use for pods/exec: ExecConfig when set,
// otherwise Config.
func (c *Client) execConfig() *rest.Config {
	if c.ExecConfig != nil {
		return c.ExecConfig
	}
	return c.Config
}

// HasKind reports whether a CRD is registered.
//
// This is the cheapest detection probe and the right one for a subsystem
// identified by its API rather than by a workload.
func (c *Client) HasKind(group, kind string) bool {
	_, err := c.Mapper.RESTMapping(schema.GroupKind{Group: group, Kind: kind})
	return err == nil
}

// HasDaemonSet reports whether a DaemonSet exists.
//
// A missing DaemonSet and a forbidden read are both reported as false: from a
// plugin's point of view a subsystem it cannot see is one it cannot render, and
// the difference belongs in a diagnostic rather than in the decision.
func (c *Client) HasDaemonSet(ctx context.Context, namespace, name string) bool {
	_, err := c.Typed.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// HasStatefulSetPrefix reports whether any StatefulSet in a namespace starts with
// prefix, for subsystems that name several related sets.
func (c *Client) HasStatefulSetPrefix(ctx context.Context, namespace, prefix string) bool {
	list, err := c.Typed.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for _, s := range list.Items {
		if strings.HasPrefix(s.Name, prefix) {
			return true
		}
	}
	return false
}

// PodCandidates returns the pods matching selector that are worth trying an exec
// against, best first. match, when non-nil, further filters by pod.
//
// The ordering and the exclusions are the point, and each one comes from a way
// this went wrong during a live upgrade:
//
// A pod on a node that has just gone down keeps phase Running for minutes. The
// phase records what the kubelet last reported, not whether anything can still
// reach the pod, so a phase-only filter hands back precisely the pod whose exec
// will time out — while its healthy replicas sit further down the list. Ready is
// the better signal, because the node lifecycle controller flips a pod's Ready
// condition when its node stops reporting.
//
// A terminating pod is worse than a dead one: it is Running, it is doomed, and an
// exec against it can succeed once and fail on the next poll. Those are excluded
// outright at any readiness.
//
// Running-but-not-Ready pods are kept as a last resort rather than dropped, for
// images that declare no readiness probe at all. They sort last, so they are only
// reached when nothing better exists.
func (c *Client) PodCandidates(ctx context.Context, namespace, selector string,
	match func(*corev1.Pod) bool) ([]string, error) {
	list, err := c.Typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s matching %q: %w", namespace, selector, err)
	}

	var ready, running []string
	for i := range list.Items {
		p := &list.Items[i]
		if match != nil && !match(p) {
			continue
		}
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if podReady(p) {
			ready = append(ready, p.Name)
		} else {
			running = append(running, p.Name)
		}
	}

	out := make([]string, 0, len(ready)+len(running))
	out = append(out, ready...)
	out = append(out, running...)
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable pod matches %q in namespace %s", selector, namespace)
	}
	return out, nil
}

// podReady reports whether the pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// RunningPod returns the best pod to exec into, by [Client.PodCandidates].
func (c *Client) RunningPod(ctx context.Context, namespace, selector string) (string, error) {
	pods, err := c.PodCandidates(ctx, namespace, selector, nil)
	if err != nil {
		return "", err
	}
	return pods[0], nil
}

// ExecFirstOf runs command in the first pod that answers, and reports which.
//
// Trying the next candidate is the whole reason this exists: one unreachable pod
// must not cost a pane its detail while healthy replicas stand beside it. That is
// what happens during a rolling upgrade, every time, as each node goes down in
// turn.
//
// A Forbidden failure stops the loop immediately. Permission to exec is a
// property of the caller, not of the pod, so trying the other twenty would fail
// identically and only delay the answer.
func (c *Client) ExecFirstOf(ctx context.Context, namespace string, pods []string,
	container string, command []string) (out string, pod string, err error) {
	if len(pods) == 0 {
		return "", "", fmt.Errorf("no candidate pods in namespace %s", namespace)
	}

	// Each attempt is bounded and the number of attempts is capped, because trying
	// candidates in turn multiplies a hang by the number of pods. An exec against a
	// pod on a node that has stopped answering does not fail, it waits; a fleet of
	// twenty agents would then turn one unreachable node into minutes of a pane
	// showing nothing. Three attempts covers the case this exists for — a replica
	// or two lost mid-rollout — and a subsystem where three consecutive candidates
	// are all unreachable has a problem the next one will not fix.
	tried := 0
	for _, name := range pods {
		if tried == maxExecAttempts {
			break
		}
		tried++

		attemptCtx, cancel := context.WithTimeout(ctx, execAttemptTimeout)
		out, err = c.Exec(attemptCtx, namespace, name, container, command)
		cancel()

		if err == nil {
			return out, name, nil
		}
		if Forbidden(err) {
			// Permission is not a property of the pod, so the others would fail
			// identically.
			return "", name, err
		}
	}
	return "", "", fmt.Errorf("no pod in %s answered %v (tried %d of %d): %w",
		namespace, command, tried, len(pods), err)
}

// Forbidden reports whether err is an exec failure caused by the caller lacking
// pods/exec permission.
//
// Use it to *explain* a failure, never to stop retrying one. It is tempting to
// treat a denial as final — permission looks like a property of the caller rather
// than of the moment — and that is wrong for the case this tool exists to watch: a
// control-plane upgrade can reissue the apiserver's kubelet client certificate
// without the group binding that authorizes exec, which forbids exec cluster-wide
// until an operator binds it back. That is a denial that absolutely does fix
// itself, mid-session, and a dashboard that remembered it would stay dark over a
// cluster that had recovered.
//
// It is still worth distinguishing, for two reasons: the message should say
// "permission" rather than "unreachable", and [Client.ExecFirstOf] stops after one
// denial instead of trying twenty pods that will fail identically.
func Forbidden(err error) bool {
	var ee *ExecError
	if errors.As(err, &ee) {
		return ee.Forbidden
	}
	return false
}

// ExecError distinguishes why a command could not be run, so a caller can pick a
// tier rather than only report a failure.
type ExecError struct {
	Pod       string
	Command   []string
	Err       error
	Stderr    string
	Forbidden bool
}

func (e *ExecError) Error() string {
	msg := fmt.Sprintf("exec %v in %s: %v", e.Command, e.Pod, e.Err)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	if e.Forbidden {
		msg += " (no pods/exec permission)"
	}
	return msg
}

func (e *ExecError) Unwrap() error { return e.Err }

// Exec runs a command in a pod and returns its stdout.
//
// Only read-only status commands belong here. The command is always a fixed
// argument list built by the plugin, never assembled from user input — that is
// what keeps a granted pods/exec permission from becoming an arbitrary-execution
// path.
func (c *Client) Exec(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
	req := c.Typed.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(c.execConfig(), "POST", req.URL())
	if err != nil {
		return "", &ExecError{Pod: pod, Command: command, Err: fmt.Errorf("spdy executor: %w", err)}
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", &ExecError{
			Pod:       pod,
			Command:   command,
			Err:       err,
			Stderr:    strings.TrimSpace(stderr.String()),
			Forbidden: apierrors.IsForbidden(err) || strings.Contains(err.Error(), "forbidden"),
		}
	}
	return stdout.String(), nil
}

// ExecProbe reports whether exec works against this pod, and whether a failure
// was a permission denial.
//
// The second return value is what keeps a transient failure from being recorded
// as a verdict. Probing a pod whose node has just gone down fails exactly like a
// missing RBAC rule from the caller's point of view, and a detector that cannot
// tell them apart pins the plugin to informer-only for the whole session and
// blames the operator's permissions for a dead node.
//
// `true` is the command: it exists in essentially every image and costs nothing.
func (c *Client) ExecProbe(ctx context.Context, namespace, pod, container string) (ok, forbidden bool) {
	probeCtx, cancel := context.WithTimeout(ctx, execProbeTimeout)
	defer cancel()
	_, err := c.Exec(probeCtx, namespace, pod, container, []string{"true"})
	if err == nil {
		return true, false
	}
	return false, Forbidden(err)
}

// CanExec probes whether exec works at all, by running a trivially cheap command.
//
// Prefer [Client.ExecProbe], which also says whether the failure was a permission
// denial — a distinction a caller needs to decide whether to retry.
func (c *Client) CanExec(ctx context.Context, namespace, pod, container string) bool {
	ok, _ := c.ExecProbe(ctx, namespace, pod, container)
	return ok
}

// execProbeTimeout bounds the exec capability probe. Detection runs before the
// dashboard appears, so it must not be able to stall startup.
const execProbeTimeout = 5 * time.Second

// execAttemptTimeout bounds one attempt in [Client.ExecFirstOf], and
// maxExecAttempts bounds how many are made. A status command answers in
// milliseconds when it answers at all; the timeout is for the case where the pod
// is gone and nothing will answer.
const (
	execAttemptTimeout = 5 * time.Second
	maxExecAttempts    = 3
)

// cachedDiscovery adapts a discovery client to the deferred RESTMapper's
// interface. Reporting itself as always fresh is correct for a short-lived
// observer: the API surface does not change mid-session.
type cachedDiscovery struct {
	discovery.DiscoveryInterface
}

func (cachedDiscovery) Fresh() bool { return true }

func (cachedDiscovery) Invalidate() {}

func (c cachedDiscovery) WithLegacy() discovery.DiscoveryInterface { return c.DiscoveryInterface }
