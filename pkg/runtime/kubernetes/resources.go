package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

var _ rt.ResourceSampler = (*Runtime)(nil)

type measuredPod struct {
	space       string
	app         string
	namespace   string
	name        string
	instanceID  string
	podUID      string
	containerID string
}

// ResourceSamples joins pod usage from metrics.k8s.io with the web container
// identities from the core API. The resource metrics API deliberately
// provides only fresh snapshots; donkey's telemetry service turns those
// snapshots into retained history.
func (r *Runtime) ResourceSamples(ctx context.Context) ([]rt.ResourceSample, error) {
	var samples []rt.ResourceSample
	var failures []error
	for _, namespace := range r.workloadNamespaces() {
		pods, err := r.c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s",
				manifests.LabelInstallation, r.c.installation(),
				manifests.LabelRole, manifests.RoleWorkload, manifests.LabelApp),
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		measured := make(map[string]measuredPod)
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || !r.c.ours(pod.Labels) {
				continue
			}
			var web *corev1.Container
			for j := range pod.Spec.Containers {
				if pod.Spec.Containers[j].Name == manifests.WebContainerName {
					web = &pod.Spec.Containers[j]
					break
				}
			}
			if web == nil {
				continue
			}
			space := pod.Labels[manifests.LabelSpace]
			app := pod.Labels[manifests.LabelApp]
			if space == "" || app == "" || r.c.requireOwnedObject("Pod", pod.Namespace, pod.Name, pod.Labels,
				manifests.Labels(space, app, manifests.RoleWorkload)) != nil {
				continue
			}
			containerID := ""
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == manifests.WebContainerName {
					containerID = status.ContainerID
					if _, value, found := strings.Cut(containerID, "://"); found {
						containerID = value
					}
					break
				}
			}
			podUID := string(pod.UID)
			instanceID := podUID
			if instanceID == "" {
				instanceID = pod.Namespace + "/" + pod.Name
			}
			measured[pod.Namespace+"/"+pod.Name] = measuredPod{
				space: space, app: app, namespace: pod.Namespace, name: pod.Name,
				instanceID: instanceID, podUID: podUID,
				containerID: containerID,
			}
		}
		if len(measured) == 0 {
			continue
		}

		metrics, err := r.c.listPodMetrics(ctx, namespace)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for i := range metrics.Items {
			item := &metrics.Items[i]
			itemNamespace := item.GetNamespace()
			if itemNamespace == "" && namespace != metav1.NamespaceAll {
				itemNamespace = namespace
			}
			pod, ok := measured[itemNamespace+"/"+item.GetName()]
			if !ok {
				continue
			}
			cpu, memory, at, ok := podMetricUsage(item, manifests.WebContainerName)
			if !ok {
				continue
			}
			if at.IsZero() {
				at = time.Now().UTC()
			}
			samples = append(samples, rt.ResourceSample{
				Space: pod.space, App: pod.app, SampledAt: at,
				Instance: pod.instanceID, ContainerID: pod.containerID, ContainerName: manifests.WebContainerName,
				KubeNamespace: pod.namespace, KubePodName: pod.name, KubePodUID: pod.podUID,
				CPUCores: cpu, MemoryBytes: memory,
			})
		}
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Space != samples[j].Space {
			return samples[i].Space < samples[j].Space
		}
		return samples[i].App < samples[j].App
	})
	return samples, errors.Join(failures...)
}

func (c *Client) listPodMetrics(ctx context.Context, namespace string) (*unstructured.UnstructuredList, error) {
	versions := []string{"v1", "v1beta1"}
	var last error
	for _, version := range versions {
		gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: version, Resource: "pods"}
		// Metrics Server filters by the source pods' labels, so a cluster-wide
		// sample need not return every other installation's pod metrics.
		list, err := c.Dynamic.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s",
				manifests.LabelInstallation, c.installation(), manifests.LabelRole, manifests.RoleWorkload, manifests.LabelApp),
		})
		if err == nil {
			return list, nil
		}
		last = err
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("reading Kubernetes pod metrics: %w", err)
		}
	}
	return nil, fmt.Errorf("Kubernetes CPU and memory metrics are unavailable; install Metrics Server or another metrics.k8s.io provider: %w", last)
}

func podMetricUsage(metric *unstructured.Unstructured, containerName string) (cpu float64, memory int64, at time.Time, ok bool) {
	containers, found, err := unstructured.NestedSlice(metric.Object, "containers")
	if err != nil || !found {
		return 0, 0, time.Time{}, false
	}
	for _, raw := range containers {
		container, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		name, _, _ := unstructured.NestedString(container, "name")
		if name != containerName {
			continue
		}
		cpuRaw, cpuFound, _ := unstructured.NestedString(container, "usage", "cpu")
		memoryRaw, memoryFound, _ := unstructured.NestedString(container, "usage", "memory")
		if !cpuFound || !memoryFound {
			return 0, 0, time.Time{}, false
		}
		cpuQuantity, cpuErr := resource.ParseQuantity(cpuRaw)
		memoryQuantity, memoryErr := resource.ParseQuantity(memoryRaw)
		if cpuErr != nil || memoryErr != nil || cpuQuantity.Sign() < 0 || memoryQuantity.Sign() < 0 {
			return 0, 0, time.Time{}, false
		}
		atRaw, _, _ := unstructured.NestedString(metric.Object, "timestamp")
		at, _ = time.Parse(time.RFC3339Nano, atRaw)
		return cpuQuantity.AsApproximateFloat64(), memoryQuantity.Value(), at.UTC(), true
	}
	return 0, 0, time.Time{}, false
}
