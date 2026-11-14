package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// nodesHidden is why a confined installation lists no nodes. Nodes are
// cluster-scoped, and an installation living in namespaces it was given holds
// nothing cluster-scoped on purpose: not being able to look is the
// configuration working, not a fault.
const nodesHidden = "This installation runs in namespaces it was given and holds no cluster-wide permission, so the cluster's nodes are not its to see."

func (r *Runtime) Info(ctx context.Context) (rt.Info, error) {
	info := rt.Info{Name: "Kubernetes"}
	version, err := r.c.Clientset.Discovery().ServerVersion()
	if err != nil {
		return info, fmt.Errorf("reading the cluster's version: %w", err)
	}
	if version != nil {
		info.Version = version.GitVersion
	}
	return info, nil
}

// Nodes reads the cluster's nodes, what the metrics API says each uses, and
// what the pods placed on each have asked for. Usage and reservations are
// each left out when their source does not answer: a cluster without a
// metrics API still shows its nodes and how full the scheduler thinks they
// are.
func (r *Runtime) Nodes(ctx context.Context) ([]rt.Node, error) {
	if r.c.Confined() {
		return nil, rt.Unavailable{Reason: nodesHidden}
	}
	list, err := r.c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return nil, rt.Unavailable{Reason: nodesHidden}
	}
	if err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	usage := r.c.nodeUsage(ctx)
	placed := r.c.podsByNode(ctx)

	nodes := make([]rt.Node, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		node := rt.Node{
			Name:        item.Name,
			Schedulable: !item.Spec.Unschedulable,
			Roles:       nodeRoles(item.Labels),
			Version:     item.Status.NodeInfo.KubeletVersion,
			OS:          item.Status.NodeInfo.OSImage,
			Arch:        item.Status.NodeInfo.Architecture,
			CreatedAt:   item.CreationTimestamp.Time,
		}
		node.Ready, node.Problems = nodeConditions(item.Status.Conditions)
		node.CPU.Capacity = nodeQuantity(item, corev1.ResourceCPU).AsApproximateFloat64()
		node.Memory.Capacity = float64(nodeQuantity(item, corev1.ResourceMemory).Value())
		node.InstanceCapacity = int(nodeQuantity(item, corev1.ResourcePods).Value())
		if used, ok := usage[item.Name]; ok {
			node.CPU.Used, node.Memory.Used = &used.cpu, &used.memory
		}
		if placed != nil {
			// Listed without this node's pods still means none are placed here.
			on := placed[item.Name]
			node.Instances = on.pods
			node.CPU.Reserved, node.Memory.Reserved = &on.cpu, &on.memory
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// nodeQuantity is what a node offers workloads: allocatable, which already
// leaves out what the kubelet and the system keep, or capacity where a node
// reports none.
func nodeQuantity(node *corev1.Node, name corev1.ResourceName) *resource.Quantity {
	if q, ok := node.Status.Allocatable[name]; ok {
		return &q
	}
	q := node.Status.Capacity[name]
	return &q
}

// nodeRoles reads the role labels kubeadm and every managed offering set.
func nodeRoles(labels map[string]string) []string {
	const prefix = "node-role.kubernetes.io/"
	var roles []string
	for key := range labels {
		if role, ok := strings.CutPrefix(key, prefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	if role := labels["kubernetes.io/role"]; role != "" && len(roles) == 0 {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// nodeConditions says whether the kubelet reports the node ready, and names
// what is wrong: a pressure that is on, or why the node is not ready.
func nodeConditions(conditions []corev1.NodeCondition) (ready bool, problems []string) {
	reported := false
	for _, condition := range conditions {
		if condition.Type == corev1.NodeReady {
			reported = true
			ready = condition.Status == corev1.ConditionTrue
			if !ready {
				problems = append(problems, strings.TrimSpace("NotReady: "+firstText(condition.Message, condition.Reason)))
			}
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			problems = append(problems, string(condition.Type))
		}
	}
	if !reported {
		problems = append(problems, "NotReady: the node has not reported its status")
	}
	return ready, problems
}

// firstText is the first of the values that says anything, cut to one line:
// the API's message where it has one, its reason otherwise.
func firstText(values ...string) string {
	for _, value := range values {
		if line, _, _ := strings.Cut(strings.TrimSpace(value), "\n"); line != "" {
			return line
		}
	}
	return ""
}

type nodeUse struct{ cpu, memory float64 }

// nodeUsage is what each node uses right now, by name; empty when the
// cluster serves no metrics API, or it has nothing to say yet.
func (c *Client) nodeUsage(ctx context.Context) map[string]nodeUse {
	usage := map[string]nodeUse{}
	for _, version := range []string{"v1", "v1beta1"} {
		gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: version, Resource: "nodes"}
		list, err := c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			cpuRaw, cpuFound, _ := unstructured.NestedString(item.Object, "usage", "cpu")
			memoryRaw, memoryFound, _ := unstructured.NestedString(item.Object, "usage", "memory")
			if !cpuFound || !memoryFound {
				continue
			}
			cpu, cpuErr := resource.ParseQuantity(cpuRaw)
			memory, memoryErr := resource.ParseQuantity(memoryRaw)
			if cpuErr != nil || memoryErr != nil || cpu.Sign() < 0 || memory.Sign() < 0 {
				continue
			}
			usage[item.GetName()] = nodeUse{cpu: cpu.AsApproximateFloat64(), memory: float64(memory.Value())}
		}
		if len(usage) > 0 {
			break
		}
	}
	return usage
}

type nodePlacement struct {
	pods        int
	cpu, memory float64
}

// podsByNode sums, per node, the pods placed on it and what they asked for:
// what the scheduler counts against the node. It reads every namespace,
// since a node is as full as all of its pods make it, in pages so a large
// cluster is never one response. Nil when the pods cannot be listed.
func (c *Client) podsByNode(ctx context.Context) map[string]nodePlacement {
	placed := map[string]nodePlacement{}
	options := metav1.ListOptions{
		// Finished pods hold nothing: the scheduler has released them.
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
		Limit:         500,
	}
	for {
		pods, err := c.Clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return nil
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			on := placed[pod.Spec.NodeName]
			on.pods++
			on.cpu += podRequest(pod, corev1.ResourceCPU).AsApproximateFloat64()
			on.memory += float64(podRequest(pod, corev1.ResourceMemory).Value())
			placed[pod.Spec.NodeName] = on
		}
		if pods.Continue == "" {
			return placed
		}
		options.Continue = pods.Continue
	}
}

// podRequest is what the scheduler holds for a pod: its containers together,
// or its largest init container when that asks for more (they run one at a
// time, before the rest), plus the pod's own overhead.
func podRequest(pod *corev1.Pod, name corev1.ResourceName) *resource.Quantity {
	total := resource.Quantity{}
	for i := range pod.Spec.Containers {
		if q, ok := pod.Spec.Containers[i].Resources.Requests[name]; ok {
			total.Add(q)
		}
	}
	for i := range pod.Spec.InitContainers {
		if q, ok := pod.Spec.InitContainers[i].Resources.Requests[name]; ok && q.Cmp(total) > 0 {
			total = q.DeepCopy()
		}
	}
	if q, ok := pod.Spec.Overhead[name]; ok {
		total.Add(q)
	}
	return &total
}

// Components reads the control plane's own Deployments and their pods, in
// the one namespace it lives in. Machines are people's, and are left out.
func (r *Runtime) Components(ctx context.Context) ([]rt.Component, error) {
	namespace := r.opts.SystemNamespace
	options := metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s,!%s,!%s,!%s,!%s",
		manifests.LabelInstallation, r.c.installation(),
		manifests.LabelPartOf, manifests.ManagedByValue,
		manifests.LabelRole,
		manifests.LabelMachine, manifests.LabelSpace, manifests.LabelApp, manifests.LabelAddon)}
	deployments, err := r.c.Clientset.AppsV1().Deployments(namespace).List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("listing the control plane's workloads: %w", err)
	}
	// Pods say what a Deployment's numbers do not: restarts, and why one is
	// not ready. Without them the components are still listed.
	podsByRole := map[string][]*corev1.Pod{}
	if pods, err := r.c.Clientset.CoreV1().Pods(namespace).List(ctx, options); err == nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			podsByRole[pod.Labels[manifests.LabelRole]] = append(podsByRole[pod.Labels[manifests.LabelRole]], pod)
		}
	}

	components := make([]rt.Component, 0, len(deployments.Items))
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		component := rt.Component{
			Name:  dep.Name,
			Role:  dep.Labels[manifests.LabelRole],
			Want:  1,
			Ready: dep.Status.AvailableReplicas,
		}
		if dep.Spec.Replicas != nil {
			component.Want = *dep.Spec.Replicas
		}
		if containers := dep.Spec.Template.Spec.Containers; len(containers) > 0 {
			component.Image = containers[0].Image
		}
		for _, pod := range podsByRole[component.Role] {
			if pod.DeletionTimestamp != nil {
				continue
			}
			for _, status := range pod.Status.ContainerStatuses {
				component.Restarts += status.RestartCount
				if component.Message == "" && !status.Ready {
					component.Message = containerProblem(status)
				}
			}
			if component.Message == "" && pod.Status.Phase == corev1.PodPending {
				component.Message = pendingReason(pod)
			}
			if pod.CreationTimestamp.Time.After(component.StartedAt) {
				component.StartedAt = pod.CreationTimestamp.Time
			}
		}
		if component.Healthy() {
			// A message about a pod that is being replaced is not news while
			// every instance the component needs is up.
			component.Message = ""
		}
		components = append(components, component)
	}
	rt.SortComponents(components)
	return components, nil
}

// containerProblem says in the kubelet's words why a container is not ready.
func containerProblem(status corev1.ContainerStatus) string {
	switch {
	case status.State.Waiting != nil:
		return firstText(status.State.Waiting.Reason, status.State.Waiting.Message)
	case status.State.Terminated != nil:
		reason := firstText(status.State.Terminated.Reason, "Terminated")
		return fmt.Sprintf("%s (exit %d)", reason, status.State.Terminated.ExitCode)
	}
	return "not ready"
}

// pendingReason is why a pod has not started: usually that no node takes it.
func pendingReason(pod *corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return firstText(condition.Message, condition.Reason, "Pending")
		}
	}
	return "Pending"
}
