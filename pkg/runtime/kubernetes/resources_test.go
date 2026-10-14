package kubernetes

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestPodMetricUsage(t *testing.T) {
	metric := &unstructured.Unstructured{Object: map[string]any{
		"timestamp": "2026-08-28T12:34:56Z",
		"containers": []any{
			map[string]any{"name": "sidecar", "usage": map[string]any{"cpu": "1", "memory": "1Gi"}},
			map[string]any{"name": "web", "usage": map[string]any{"cpu": "125m", "memory": "96Mi"}},
		},
	}}
	cpu, memory, at, ok := podMetricUsage(metric, "web")
	if !ok {
		t.Fatal("web usage was not parsed")
	}
	if math.Abs(cpu-0.125) > 1e-9 || memory != 96*1024*1024 {
		t.Fatalf("usage CPU=%v memory=%d", cpu, memory)
	}
	want := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("timestamp=%s, want %s", at, want)
	}
}

func TestResourceSamplesFindInstallationStampedAppPods(t *testing.T) {
	const (
		installation = "bank-donkey-system"
		namespace    = "bank-apps"
	)
	labels := manifests.Labels("team", "site", manifests.RoleWorkload)
	labels[manifests.LabelInstallation] = installation
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "site-abc", Namespace: namespace, UID: types.UID("pod-uid"), Labels: labels,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: manifests.WebContainerName}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: manifests.WebContainerName, ContainerID: "containerd://container-id",
			}},
		},
	}
	foreign := pod.DeepCopy()
	foreign.Name = "foreign-abc"
	foreign.UID = types.UID("foreign-uid")
	foreign.Labels = mapsClone(labels)
	foreign.Labels[manifests.LabelInstallation] = "other-donkey-system"

	metricGVR := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1", Resource: "pods"}
	metric := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metrics.k8s.io/v1", "kind": "PodMetrics",
			"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{
				manifests.LabelInstallation: installation,
				manifests.LabelRole:         manifests.RoleWorkload,
				manifests.LabelApp:          "site",
			}},
			"timestamp": "2026-08-28T12:34:56Z",
			"containers": []any{map[string]any{
				"name":  manifests.WebContainerName,
				"usage": map[string]any{"cpu": "125m", "memory": "96Mi"},
			}},
		}}
	}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{metricGVR: "PodMetricsList"},
	)
	for _, object := range []*unstructured.Unstructured{metric(pod.Name), metric(foreign.Name)} {
		if _, err := dynamic.Resource(metricGVR).Namespace(namespace).Create(
			context.Background(), object, metav1.CreateOptions{},
		); err != nil {
			t.Fatal(err)
		}
	}
	clientset := fake.NewClientset(pod, foreign)
	runtime := NewRuntime(&Client{Clientset: clientset, Dynamic: dynamic}, Options{
		SystemNamespace: installation,
		Placement:       manifests.Placement{SharedNamespace: namespace},
	})
	samples, err := runtime.ResourceSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples=%+v, want exactly this installation's pod", samples)
	}
	got := samples[0]
	if got.Space != "team" || got.App != "site" || got.Instance != "pod-uid" ||
		got.ContainerID != "container-id" || got.CPUCores != 0.125 || got.MemoryBytes != 96*1024*1024 {
		t.Fatalf("sample=%+v", got)
	}
	var metricLists int
	for _, action := range dynamic.Actions() {
		if list, ok := action.(k8stesting.ListAction); ok {
			metricLists++
			selector := list.GetListRestrictions().Labels.String()
			if !strings.Contains(selector, manifests.LabelInstallation+"="+installation) ||
				!strings.Contains(selector, manifests.LabelRole+"="+manifests.RoleWorkload) || action.GetNamespace() != namespace {
				t.Fatalf("metrics read was not confined to installation app pods: %s in %q", selector, action.GetNamespace())
			}
		}
	}
	if metricLists != 1 {
		t.Fatalf("metrics lists = %d, want one workload read", metricLists)
	}
}

func mapsClone(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
