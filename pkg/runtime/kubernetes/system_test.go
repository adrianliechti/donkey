package kubernetes

import (
	"context"
	"errors"
	"math"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

func nodeMetricsClient(t *testing.T, usage map[string][2]string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
			nodeMetricsGVR: "NodeMetricsList",
			{Group: "metrics.k8s.io", Version: "v1", Resource: "nodes"}: "NodeMetricsList",
		},
	)
	for name, used := range usage {
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
			"metadata": map[string]any{"name": name},
			"usage":    map[string]any{"cpu": used[0], "memory": used[1]},
		}}
		if _, err := dynamic.Resource(nodeMetricsGVR).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return dynamic
}

func quantities(cpu, memory, pods string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
		corev1.ResourcePods:   resource.MustParse(pods),
	}
}

func placedPod(name, node string, phase corev1.PodPhase, containers, inits []corev1.ResourceList) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "anywhere"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
	for _, requests := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Resources: corev1.ResourceRequirements{Requests: requests},
		})
	}
	for _, requests := range inits {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Resources: corev1.ResourceRequirements{Requests: requests},
		})
	}
	return pod
}

func TestNodesReportCapacityUsageAndReservations(t *testing.T) {
	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Status: corev1.NodeStatus{
			// Allocatable, not capacity, is what workloads can have.
			Capacity:    quantities("4", "8Gi", "110"),
			Allocatable: quantities("3500m", "7Gi", "110"),
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.2", OSImage: "Ubuntu 24.04", Architecture: "arm64"},
		},
	}
	troubled := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Capacity: quantities("2", "4Gi", "110"),
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Reason: "NodeStatusUnknown", Message: "Kubelet stopped posting node status."},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
			},
		},
	}
	half := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}
	whole := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	clientset := fake.NewClientset(ready, troubled,
		// Two containers, and an init container that asks for more CPU than
		// both: the scheduler holds the larger of the two.
		placedPod("web", "node-a", corev1.PodRunning, []corev1.ResourceList{half, {corev1.ResourceCPU: resource.MustParse("250m")}}, []corev1.ResourceList{whole}),
		placedPod("worker", "node-a", corev1.PodPending, []corev1.ResourceList{half}, nil),
		// Finished and unplaced pods hold nothing on any node.
		placedPod("job", "node-a", corev1.PodSucceeded, []corev1.ResourceList{whole}, nil),
		placedPod("waiting", "", corev1.PodPending, []corev1.ResourceList{whole}, nil),
	)
	runtime := NewRuntime(&Client{
		Clientset: clientset,
		Dynamic:   nodeMetricsClient(t, map[string][2]string{"node-a": {"1250m", "3Gi"}}),
	}, Options{})

	nodes, err := runtime.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Name != "node-a" || nodes[1].Name != "node-b" {
		t.Fatalf("nodes: %+v", nodes)
	}
	a := nodes[0]
	if !a.Ready || !a.Schedulable || len(a.Problems) != 0 || len(a.Roles) != 1 || a.Roles[0] != "control-plane" ||
		a.Version != "v1.31.2" || a.OS != "Ubuntu 24.04" || a.Arch != "arm64" {
		t.Fatalf("node-a state: %+v", a)
	}
	if a.CPU.Capacity != 3.5 || a.Memory.Capacity != 7*1024*1024*1024 || a.InstanceCapacity != 110 || a.Instances != 2 {
		t.Fatalf("node-a capacity: %+v", a)
	}
	if a.CPU.Used == nil || math.Abs(*a.CPU.Used-1.25) > 1e-9 || a.Memory.Used == nil || *a.Memory.Used != 3*1024*1024*1024 {
		t.Fatalf("node-a usage: %+v %+v", a.CPU, a.Memory)
	}
	// web holds 1 CPU (its init container) and 256Mi; worker 500m and 256Mi.
	if a.CPU.Reserved == nil || math.Abs(*a.CPU.Reserved-1.5) > 1e-9 || a.Memory.Reserved == nil || *a.Memory.Reserved != 512*1024*1024 {
		t.Fatalf("node-a reservations: %+v %+v", a.CPU, a.Memory)
	}

	b := nodes[1]
	if b.Ready || b.Schedulable || len(b.Problems) != 2 ||
		b.Problems[0] != "NotReady: Kubelet stopped posting node status." || b.Problems[1] != "DiskPressure" {
		t.Fatalf("node-b state: %+v", b)
	}
	// No metrics for node-b: its usage is unknown, not zero. Nothing is
	// placed on it, which is known.
	if b.CPU.Capacity != 2 || b.CPU.Used != nil || b.Memory.Used != nil || b.CPU.Reserved == nil || *b.CPU.Reserved != 0 || b.Instances != 0 {
		t.Fatalf("node-b resources: %+v %+v", b.CPU, b.Memory)
	}
}

func TestNodesWithoutMetricsStillListCapacity(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Allocatable: quantities("4", "8Gi", "110"),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	// A cluster without a metrics API answers "no such resource".
	dynamic := nodeMetricsClient(t, nil)
	dynamic.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(nodeMetricsGVR.GroupResource(), "")
	})
	runtime := NewRuntime(&Client{Clientset: fake.NewClientset(node), Dynamic: dynamic}, Options{})
	nodes, err := runtime.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !nodes[0].Ready || nodes[0].CPU.Capacity != 4 || nodes[0].CPU.Used != nil || nodes[0].Memory.Used != nil {
		t.Fatalf("nodes: %+v", nodes)
	}
}

// An installation confined to namespaces it was given holds nothing
// cluster-scoped: it says it cannot see the nodes, and never asks.
func TestNodesAreNotAConfinedInstallationsToSee(t *testing.T) {
	clientset := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	runtime := NewRuntime(&Client{Clientset: clientset, Dynamic: dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme())}, Options{
		SystemNamespace: "bank-donkey-system",
		Placement:       manifests.Placement{SharedNamespace: "bank-apps"},
	})
	nodes, err := runtime.Nodes(context.Background())
	var unavailable rt.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Reason == "" || nodes != nil {
		t.Fatalf("confined nodes: %+v %v", nodes, err)
	}
	if len(clientset.Actions()) != 0 {
		t.Fatalf("a confined installation asked the cluster anyway: %+v", clientset.Actions())
	}
}

func systemDeployment(installation, name, image string, want, available int32) *appsv1.Deployment {
	labels := manifests.SystemLabels(name)
	labels[manifests.LabelInstallation] = installation
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: installation, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &want,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}}},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func systemPod(installation, name, pod string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	labels := manifests.SystemLabels(name)
	labels[manifests.LabelInstallation] = installation
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod, Namespace: installation, Labels: labels,
			CreationTimestamp: metav1.Date(2026, 11, 1, 8, 0, 0, 0, metav1.Now().Location()),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}
}

func TestComponentsAreTheControlPlanesOwnWorkloads(t *testing.T) {
	const installation = "donkey-system"
	machineLabels := manifests.MachineLabels("u-abc", manifests.RoleMachine)
	machineLabels[manifests.LabelInstallation] = installation
	machine := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "machine-u-abc", Namespace: installation, Labels: machineLabels}}
	// Another installation's gateway in the same namespace is not this one's.
	foreign := systemDeployment("other-donkey", manifests.GatewayAppName, "donkey:1", 1, 1)
	foreign.Namespace, foreign.Name = installation, "other-gateway"

	clientset := fake.NewClientset(
		systemDeployment(installation, manifests.GatewayAppName, "ghcr.io/acme/donkey-server:0.4.0", 2, 1),
		systemDeployment(installation, manifests.ServerName, "ghcr.io/acme/donkey-server:0.4.0", 1, 1),
		systemPod(installation, manifests.ServerName, "donkey-server-1", corev1.ContainerStatus{Ready: true, RestartCount: 2}),
		systemPod(installation, manifests.GatewayAppName, "donkey-gateway-1", corev1.ContainerStatus{Ready: true}),
		systemPod(installation, manifests.GatewayAppName, "donkey-gateway-2", corev1.ContainerStatus{
			RestartCount: 7,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}),
		machine, foreign,
	)
	runtime := NewRuntime(&Client{Clientset: clientset}, Options{SystemNamespace: installation})
	components, err := runtime.Components(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 {
		t.Fatalf("components: %+v", components)
	}
	// The same order on every runtime: the server before the gateway.
	server, gateway := components[0], components[1]
	if server.Name != manifests.ServerName || server.Role != "server" || !server.Healthy() ||
		server.Restarts != 2 || server.Message != "" || server.Image != "ghcr.io/acme/donkey-server:0.4.0" || server.StartedAt.IsZero() {
		t.Fatalf("server: %+v", server)
	}
	if gateway.Role != "gateway" || gateway.Healthy() || gateway.Ready != 1 || gateway.Want != 2 ||
		gateway.Restarts != 7 || gateway.Message != "CrashLoopBackOff" {
		t.Fatalf("gateway: %+v", gateway)
	}
}
