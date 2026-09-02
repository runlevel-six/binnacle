package workload

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/binnacle/internal/core/capi"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
)

func upstreamRoles() profile.NodeRoles {
	return profile.NodeRoles{LabelKeys: []string{profile.UpstreamRoleLabelPrefix + profile.WildcardSuffix}}
}

func ptr[T any](v T) *T { return &v }

// --- nodes ----------------------------------------------------------------

func TestProjectNode_ReadyAndRoles(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp-1",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          "v1.32.0",
				OSImage:                 "Ubuntu 24.04",
				KernelVersion:           "6.8.0",
				ContainerRuntimeVersion: "containerd://2.0.0",
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.99"}, // first wins
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	got := ProjectNode(n, upstreamRoles())
	if !got.Ready() || got.Status != "Ready" {
		t.Errorf("status: got %q", got.Status)
	}
	if got.Role != "control-plane" {
		t.Errorf("Role: got %q want control-plane", got.Role)
	}
	if got.Version != "v1.32.0" || got.OSImage != "Ubuntu 24.04" {
		t.Errorf("node info: got %+v", got)
	}
	if got.InternalIP != "10.0.0.1" || got.ExternalIP != "203.0.113.1" {
		t.Errorf("addresses: got internal=%q external=%q", got.InternalIP, got.ExternalIP)
	}
	if got.AllocatableCPU != 4000 {
		t.Errorf("AllocatableCPU: got %d want 4000 millicores", got.AllocatableCPU)
	}
	if got.AllocatableMemory != 8*1024*1024*1024 {
		t.Errorf("AllocatableMemory: got %d", got.AllocatableMemory)
	}
}

func TestProjectNode_StatusVariants(t *testing.T) {
	mk := func(conds ...corev1.NodeCondition) *corev1.Node {
		return &corev1.Node{Status: corev1.NodeStatus{Conditions: conds}}
	}
	tests := []struct {
		name string
		node *corev1.Node
		want string
	}{
		{"ready", mk(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}), "Ready"},
		{"not ready", mk(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}), "NotReady"},
		{"unknown", mk(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}), "Unknown"},
		// A node with no Ready condition has told us nothing; that is Unknown,
		// not healthy.
		{"no ready condition", mk(), "Unknown"},
	}
	for _, tc := range tests {
		if got := ProjectNode(tc.node, upstreamRoles()).Status; got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestProjectNode_PressureConditions(t *testing.T) {
	n := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
		{Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue},
		{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionUnknown},
	}}}

	got := ProjectNode(n, upstreamRoles())
	if !got.MemoryPressure || !got.PIDPressure {
		t.Errorf("True pressures should be set: %+v", got)
	}
	// Only True counts — False and Unknown must not raise an alarm.
	if got.DiskPressure || got.NetworkUnavail {
		t.Errorf("non-True conditions should not be set: %+v", got)
	}
}

// Cordon state is reported separately from readiness, so a pane can show both.
func TestNodeDisplayStatus(t *testing.T) {
	tests := []struct {
		status   string
		cordoned bool
		want     string
	}{
		{"Ready", false, "Ready"},
		{"Ready", true, "Cordoned"},
		{"NotReady", false, "NotReady"},
		{"NotReady", true, "NotReady,Cordoned"},
	}
	for _, tc := range tests {
		n := model.Node{Status: tc.status, Cordoned: tc.cordoned}
		if got := n.DisplayStatus(); got != tc.want {
			t.Errorf("status=%q cordoned=%v: got %q want %q", tc.status, tc.cordoned, got, tc.want)
		}
	}
}

func TestProjectNode_Cordoned(t *testing.T) {
	n := &corev1.Node{
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	got := ProjectNode(n, upstreamRoles())
	if !got.Cordoned {
		t.Error("Cordoned should mirror spec.unschedulable")
	}
	// Status stays the readiness value; cordon state is separate.
	if got.Status != "Ready" {
		t.Errorf("Status: got %q want Ready", got.Status)
	}
	if got.DisplayStatus() != "Cordoned" {
		t.Errorf("DisplayStatus: got %q want Cordoned", got.DisplayStatus())
	}
}

func TestProjectNodes_SortedByName(t *testing.T) {
	snap := ProjectNodes([]*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "z"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
	}, nil, upstreamRoles())

	var names []string
	for _, n := range snap.Items {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "a,m,z" {
		t.Errorf("got %v want sorted", names)
	}
}

// --- node resource attribution -------------------------------------------

func container(name, cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}}
	if cpuReq != "" {
		c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if memReq != "" {
		c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	if cpuLim != "" {
		c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	if memLim != "" {
		c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
	}
	return c
}

func TestProjectNodes_SumsPodRequests(t *testing.T) {
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"},
			Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{
				container("c1", "100m", "128Mi", "200m", "256Mi"),
				container("c2", "150m", "64Mi", "", ""),
			}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"},
			Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{
				container("c1", "50m", "32Mi", "", ""),
			}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}

	got := ProjectNodes(nodes, pods, upstreamRoles()).Items[0]
	if got.RequestedCPU != 300 {
		t.Errorf("RequestedCPU: got %d want 300", got.RequestedCPU)
	}
	if want := int64((128 + 64 + 32) * 1024 * 1024); got.RequestedMemory != want {
		t.Errorf("RequestedMemory: got %d want %d", got.RequestedMemory, want)
	}
	if got.LimitsCPU != 200 {
		t.Errorf("LimitsCPU: got %d want 200", got.LimitsCPU)
	}
}

// A completed or failed pod has released its resources. Counting it would
// overstate utilization, badly on a node that has run many jobs.
func TestProjectNodes_IgnoresTerminatedPods(t *testing.T) {
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "done"},
			Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{container("c", "500m", "1Gi", "", "")}},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "failed"},
			Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{container("c", "500m", "1Gi", "", "")}},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "live"},
			Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{container("c", "100m", "128Mi", "", "")}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}

	got := ProjectNodes(nodes, pods, upstreamRoles()).Items[0]
	if got.RequestedCPU != 100 {
		t.Errorf("RequestedCPU: got %d want 100 (only the running pod)", got.RequestedCPU)
	}
}

// Init containers run sequentially, so the scheduler takes their maximum, not
// their sum, and the pod's request is the greater of that and the regular sum.
func TestProjectNodes_InitContainersUseMaxNotSum(t *testing.T) {
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	pods := []*corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{container("main", "100m", "64Mi", "", "")},
			InitContainers: []corev1.Container{
				container("init-1", "500m", "256Mi", "", ""),
				container("init-2", "300m", "128Mi", "", ""),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}}

	got := ProjectNodes(nodes, pods, upstreamRoles()).Items[0]
	// max(sum(regular)=100, max(init)=500) = 500
	if got.RequestedCPU != 500 {
		t.Errorf("RequestedCPU: got %d want 500 (largest init container)", got.RequestedCPU)
	}
	if want := int64(256 * 1024 * 1024); got.RequestedMemory != want {
		t.Errorf("RequestedMemory: got %d want %d", got.RequestedMemory, want)
	}
}

func TestProjectNodes_UnscheduledPodsIgnored(t *testing.T) {
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	pods := []*corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{container("c", "500m", "1Gi", "", "")}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}}
	if got := ProjectNodes(nodes, pods, upstreamRoles()).Items[0]; got.RequestedCPU != 0 {
		t.Errorf("an unscheduled pod should not be attributed: got %d", got.RequestedCPU)
	}
}

func TestProjectNodes_NilPodsLeavesResourcesZero(t *testing.T) {
	nodes := []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	got := ProjectNodes(nodes, nil, upstreamRoles()).Items[0]
	if got.RequestedCPU != 0 || got.RequestedMemory != 0 {
		t.Errorf("expected zero resources, got %+v", got)
	}
}

// --- pods -----------------------------------------------------------------

func TestProjectPod_HealthyRunning(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "coredns-abc"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "a"}, {Name: "b"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.1.2.3",
			ContainerStatuses: []corev1.ContainerStatus{
				{Ready: true, RestartCount: 1},
				{Ready: true, RestartCount: 2},
			},
		},
	}

	got := ProjectPod(p)
	if got.Status != "Running" || !got.IsHealthy {
		t.Errorf("got status=%q healthy=%v", got.Status, got.IsHealthy)
	}
	if got.ReadyReady != 2 || got.ReadyTotal != 2 {
		t.Errorf("ready: got %d/%d want 2/2", got.ReadyReady, got.ReadyTotal)
	}
	if got.Restarts != 3 {
		t.Errorf("Restarts: got %d want 3 (summed)", got.Restarts)
	}
	if got.IP != "10.1.2.3" || got.Node != "node-1" {
		t.Errorf("got ip=%q node=%q", got.IP, got.Node)
	}
}

func TestProjectPod_PartiallyReadyIsUnhealthy(t *testing.T) {
	p := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}, {Ready: false}},
		},
	}
	got := ProjectPod(p)
	if got.IsHealthy {
		t.Error("1/2 ready should not be healthy")
	}
}

func TestProjectPod_StatusPrecedence(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "deletion wins over everything",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptr(metav1.NewTime(time.Now()))},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: "Terminating",
		},
		{
			name: "deletion surfaces a container reason when there is one",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptr(metav1.NewTime(time.Now()))},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
						},
					}},
				},
			},
			want: "OOMKilled",
		},
		{
			name: "waiting init container reports its reason",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "i1"}, {Name: "i2"}}},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
						},
					}},
				},
			},
			want: "Init:ImagePullBackOff",
		},
		{
			name: "initializing init container reports progress",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "i1"}, {Name: "i2"}}},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
						},
					}},
				},
			},
			want: "Init:0/2",
		},
		{
			name: "completed init containers are skipped",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "i1"}},
					Containers:     []corev1.Container{{Name: "c"}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					InitContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
						},
					}},
					ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
				},
			},
			want: "Running",
		},
		{
			name: "container waiting reason beats the phase",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
						},
					}},
				},
			},
			want: "CrashLoopBackOff",
		},
		{
			name: "succeeded reads as Completed",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: "Completed",
		},
		{
			name: "failed reads as Error",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			want: "Error",
		},
		{
			name: "otherwise the phase is used",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: "Pending",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectPod(tc.pod).Status; got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// A completed pod is healthy — a finished job is not a problem to surface.
func TestProjectPod_CompletedIsHealthy(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if !ProjectPod(p).IsHealthy {
		t.Error("a Completed pod should be healthy")
	}
}

// A pod with no containers must not read as "0/0 ready and therefore healthy".
func TestProjectPod_NoContainersIsNotHealthy(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if ProjectPod(p).IsHealthy {
		t.Error("a Running pod with no containers should not be healthy")
	}
}

func TestProjectPods_SortedByNamespaceThenName(t *testing.T) {
	snap := ProjectPods([]*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "z"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "a"}},
	})
	var got []string
	for _, p := range snap.Items {
		got = append(got, p.Namespace+"/"+p.Name)
	}
	if strings.Join(got, ",") != "a/a,a/z,b/a" {
		t.Errorf("got %v", got)
	}
}

// --- workloads ------------------------------------------------------------

func TestProjectDeployments(t *testing.T) {
	got := ProjectDeployments([]*appsv1.Deployment{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "coredns"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d want 1", len(got))
	}
	if got[0].Kind != KindDeployment || got[0].Ready != 1 || got[0].Desired != 2 {
		t.Errorf("got %+v", got[0])
	}
}

// A Deployment with no replicas set defaults to one, not zero. Reporting zero
// would make a healthy single-replica Deployment look like "0/0".
func TestProjectDeployments_NilReplicasDefaultsToOne(t *testing.T) {
	got := ProjectDeployments([]*appsv1.Deployment{{
		ObjectMeta: metav1.ObjectMeta{Name: "d"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}})
	if got[0].Desired != 1 {
		t.Errorf("Desired: got %d want 1", got[0].Desired)
	}
}

func TestProjectStatefulSets_NilReplicasDefaultsToOne(t *testing.T) {
	got := ProjectStatefulSets([]*appsv1.StatefulSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "s"},
	}})
	if got[0].Desired != 1 || got[0].Kind != KindStatefulSet {
		t.Errorf("got %+v", got[0])
	}
}

// A DaemonSet's size is a function of matching nodes, so desired comes from the
// scheduler's own count rather than a spec field.
func TestProjectDaemonSets_DesiredFromStatus(t *testing.T) {
	got := ProjectDaemonSets([]*appsv1.DaemonSet{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-proxy"},
		Status:     appsv1.DaemonSetStatus{NumberReady: 4, DesiredNumberScheduled: 5},
	}})
	if got[0].Kind != KindDaemonSet || got[0].Ready != 4 || got[0].Desired != 5 {
		t.Errorf("got %+v", got[0])
	}
}

func TestSortWorkloads(t *testing.T) {
	ws := []model.Workload{
		{Kind: KindStatefulSet, Namespace: "a", Name: "z"},
		{Kind: KindDaemonSet, Namespace: "b", Name: "a"},
		{Kind: KindStatefulSet, Namespace: "a", Name: "a"},
		{Kind: KindDeployment, Namespace: "z", Name: "a"},
	}
	SortWorkloads(ws)
	var got []string
	for _, w := range ws {
		got = append(got, w.Kind+":"+w.Namespace+"/"+w.Name)
	}
	want := "DaemonSet:b/a,Deployment:z/a,StatefulSet:a/a,StatefulSet:a/z"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// --- events ---------------------------------------------------------------

func TestFilterEvents(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	ev := func(ns, reason string, at time.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: ns, Name: reason},
			Reason:        reason,
			LastTimestamp: metav1.NewTime(at),
		}
	}
	raw := []*corev1.Event{
		ev("kube-system", "Old", base.Add(-time.Hour)),
		ev("default", "Ignored", base),
		ev("kube-system", "New", base),
		ev("team-alpha", "Prefixed", base.Add(-time.Minute)),
	}
	filter := profile.Events{
		Namespaces:        []string{"kube-system"},
		NamespacePrefixes: []string{"team-"},
	}

	got := FilterEvents(raw, filter, capi.ProjectEvent)
	if len(got) != 3 {
		t.Fatalf("got %d events want 3 (default excluded)", len(got))
	}
	// Newest first.
	if got[0].Reason != "New" || got[2].Reason != "Old" {
		var reasons []string
		for _, e := range got {
			reasons = append(reasons, e.Reason)
		}
		t.Errorf("order: got %v want [New Prefixed Old]", reasons)
	}
	for _, e := range got {
		if e.Namespace == "default" {
			t.Error("an excluded namespace leaked through")
		}
	}
}

// An unconfigured filter yields nothing rather than the whole cluster's events.
func TestFilterEvents_UnconfiguredFilterYieldsNothing(t *testing.T) {
	raw := []*corev1.Event{{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"}}}
	if got := FilterEvents(raw, profile.Events{}, capi.ProjectEvent); len(got) != 0 {
		t.Errorf("got %d want 0", len(got))
	}
}

func TestFilterEvents_AllNamespaces(t *testing.T) {
	raw := []*corev1.Event{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "b"}},
	}
	if got := FilterEvents(raw, profile.Events{AllNamespaces: true}, capi.ProjectEvent); len(got) != 2 {
		t.Errorf("got %d want 2", len(got))
	}
}
