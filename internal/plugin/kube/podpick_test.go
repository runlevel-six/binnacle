package kube

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// pod builds a pod in the shape the candidate rules care about.
func pod(name string, phase corev1.PodPhase, ready bool, terminating bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{"app": "agent"},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if terminating {
		now := metav1.NewTime(time.Unix(1700000000, 0))
		p.DeletionTimestamp = &now
	}
	return p
}

func client(pods ...*corev1.Pod) *Client {
	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	return &Client{Typed: fake.NewSimpleClientset(objs...)}
}

// The whole reason this exists: a pod on a node that has just gone down keeps
// phase Running, so a phase-only filter returns exactly the pod that will not
// answer. Ready pods must come first, and a terminating pod must never be offered.
func TestPodCandidates_PrefersReadyAndSkipsTerminating(t *testing.T) {
	c := client(
		pod("agent-dead", corev1.PodRunning, false, false), // node gone: Running, not Ready
		pod("agent-doomed", corev1.PodRunning, true, true), // Ready but terminating
		pod("agent-good", corev1.PodRunning, true, false),  // the one we want
		pod("agent-gone", corev1.PodSucceeded, false, false),
	)

	got, err := c.PodCandidates(context.Background(), "ns", "app=agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "agent-good" {
		t.Errorf("candidates = %v, want the Ready pod first", got)
	}
	for _, name := range got {
		if name == "agent-doomed" {
			t.Error("a terminating pod must never be offered: exec against it can " +
				"succeed once and fail on the next poll")
		}
		if name == "agent-gone" {
			t.Errorf("a non-Running pod must not be offered, got %v", got)
		}
	}
	// The not-Ready pod is kept as a last resort for images with no readiness
	// probe, but must sort after the Ready one.
	if len(got) != 2 {
		t.Errorf("candidates = %v, want the Ready pod then the Running one", got)
	}
}

// A pod with no readiness probe reports no Ready condition. It must still be
// offered when nothing better exists, or a plugin whose image declares no probe
// loses its detail entirely.
func TestPodCandidates_RunningIsUsedWhenNothingIsReady(t *testing.T) {
	c := client(pod("agent-1", corev1.PodRunning, false, false))
	got, err := c.PodCandidates(context.Background(), "ns", "app=agent", nil)
	if err != nil {
		t.Fatalf("a Running pod should still be a candidate: %v", err)
	}
	if len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("candidates = %v, want [agent-1]", got)
	}
}

func TestPodCandidates_MatchFilters(t *testing.T) {
	c := client(
		pod("ovsdb-nb-0", corev1.PodRunning, true, false),
		pod("ovsdb-sb-0", corev1.PodRunning, true, false),
	)
	got, err := c.PodCandidates(context.Background(), "ns", "", func(p *corev1.Pod) bool {
		return p.Name == "ovsdb-sb-0"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ovsdb-sb-0" {
		t.Errorf("candidates = %v, want only the matched pod", got)
	}
}

func TestPodCandidates_NoneUsable(t *testing.T) {
	c := client(pod("agent-doomed", corev1.PodRunning, true, true))
	if _, err := c.PodCandidates(context.Background(), "ns", "app=agent", nil); err == nil {
		t.Error("a namespace whose only pod is terminating has no usable candidate")
	}
}

// Forbidden has to see through wrapping, since callers compare an error that has
// traveled up through several layers.
func TestForbidden(t *testing.T) {
	denied := &ExecError{Pod: "p", Forbidden: true, Err: errors.New("forbidden")}
	if !Forbidden(denied) {
		t.Error("a Forbidden ExecError should be recognized")
	}
	if !Forbidden(fmt.Errorf("wrapped: %w", denied)) {
		t.Error("Forbidden must see through wrapping")
	}
	transient := &ExecError{Pod: "p", Err: errors.New("i/o timeout")}
	if Forbidden(transient) {
		t.Error("a timeout is not a permission denial — treating it as one is what " +
			"pinned the tier for a whole session")
	}
	if Forbidden(nil) || Forbidden(errors.New("plain")) {
		t.Error("only an ExecError carries permission information")
	}
}
