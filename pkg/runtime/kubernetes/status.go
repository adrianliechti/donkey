package kubernetes

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func (c *Client) AppStatus(ctx context.Context, namespace, app string) (*rt.AppStatus, error) {
	st := &rt.AppStatus{}
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.Found = true
	st.Image = dep.Annotations[manifests.AnnotationImage]
	if st.Image == "" && len(dep.Spec.Template.Spec.Containers) > 0 {
		st.Image = dep.Spec.Template.Spec.Containers[0].Image
	}
	if containers := dep.Spec.Template.Spec.Containers; len(containers) > 0 && len(containers[0].Ports) > 0 {
		st.Port = containers[0].Ports[0].ContainerPort
	}
	st.WantReplica = 1
	if dep.Spec.Replicas != nil {
		st.WantReplica = *dep.Spec.Replicas
	}
	st.ReadyReplica = dep.Status.AvailableReplicas
	st.UpToDate = dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas == st.WantReplica &&
		dep.Status.Replicas == st.WantReplica

	if pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(dep.Spec.Selector.MatchLabels).String(),
	}); err == nil {
		for _, p := range pods.Items {
			st.Instances = append(st.Instances, podSummary(&p))
			for _, cs := range p.Status.ContainerStatuses {
				st.Restarts += cs.RestartCount
			}
			if p.CreationTimestamp.Time.After(st.StartedAt) {
				st.StartedAt = p.CreationTimestamp.Time
			}
		}
	}
	if cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, app, metav1.GetOptions{}); err == nil {
		if cm.Annotations[manifests.AnnotationBackend] != "" {
			st.IngressHost = cm.Annotations[manifests.AnnotationHost]
			st.Protected = cm.Annotations[manifests.AnnotationLogin] == manifests.LoginRequired
		}
	}
	return st, nil
}

// DeploymentAnnotation reads one annotation off a Deployment; a missing
// Deployment (or annotation) is "".
func (c *Client) DeploymentAnnotation(ctx context.Context, namespace, name, key string) (string, error) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return dep.Annotations[key], nil
}

// ServiceEndpoint returns the in-cluster host and port of a space service
// ("" when none runs) — donkey-server sits in the cluster and can dial it
// directly, which is what the tunnel rides on.
func (c *Client) ServiceEndpoint(ctx context.Context, namespace, name string) (string, int32, error) {
	svc, err := c.Clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if len(svc.Spec.Ports) == 0 {
		return "", 0, fmt.Errorf("the service %s exposes no port", name)
	}
	return fmt.Sprintf("%s.%s.svc", name, namespace), svc.Spec.Ports[0].Port, nil
}

// DeploymentIdentity reports the stable role of a named Deployment.
func (c *Client) DeploymentIdentity(ctx context.Context, namespace, name string) (component, addon string, exists bool, err error) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return dep.Labels[manifests.LabelComponent], dep.Labels[manifests.LabelAddon], true, nil
}

// ListAddonBindings finds the app's addon binding Secrets.
// An empty app selects the space's canonical bindings, which deliberately
// have no pseudo-app label.
func (c *Client) ListAddonBindings(ctx context.Context, namespace, space, app string) ([]rt.ServiceBindingInfo, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s,%s=%s,%s",
		manifests.LabelInstallation, c.installation(), manifests.LabelSpace, space,
		manifests.LabelComponent, manifests.ComponentBinding, manifests.LabelAddon)
	if app != "" {
		selector += "," + manifests.LabelApp + "=" + app
	}
	secrets, err := c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	var out []rt.ServiceBindingInfo
	for _, sec := range secrets.Items {
		if !c.ours(sec.Labels) || (app == "") != (sec.Labels[manifests.LabelApp] == "") {
			continue
		}
		name := sec.Labels[manifests.LabelAddon]
		if name == "" {
			continue
		}
		b := rt.ServiceBindingInfo{Name: name}
		for k := range sec.Data {
			b.Keys = append(b.Keys, k)
		}
		sort.Strings(b.Keys)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SharedBindingValues collects one key across canonical space bindings.
func (c *Client) SharedBindingValues(ctx context.Context, key string) ([]string, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s,%s",
		manifests.LabelInstallation, c.installation(),
		manifests.LabelComponent, manifests.ComponentBinding, manifests.LabelAddon)
	var out []string
	for _, ns := range c.sweep() {
		secrets, err := c.Clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return nil, err
		}
		for _, sec := range secrets.Items {
			if !c.ours(sec.Labels) || sec.Labels[manifests.LabelApp] != "" {
				continue
			}
			if v, ok := sec.Data[key]; ok && len(v) > 0 {
				out = append(out, string(v))
			}
		}
	}
	return out, nil
}

// ListAddonAttachments names the apps holding a copy of the shared addon's
// binding.
//
// The space is part of the selector, not a nicety: where every space shares
// one namespace, "addon-postgres" is a component several spaces have, and
// without it removing one space's database would detach another's apps.
func (c *Client) ListAddonAttachments(ctx context.Context, namespace, space, addon string) ([]string, error) {
	secrets, err := c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s,%s=%s,%s",
			manifests.LabelInstallation, c.installation(), manifests.LabelSpace, space,
			manifests.LabelAddon, addon, manifests.LabelComponent, manifests.ComponentBinding,
			manifests.LabelApp),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sec := range secrets.Items {
		if app := sec.Labels[manifests.LabelApp]; app != "" && c.ours(sec.Labels) {
			out = append(out, app)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RolloutEvidenceFor collects the same pod summaries, problem-pod logs and
// warning events a failed rollout reports — for on-demand diagnosis of an
// app that is currently unhealthy.
func (c *Client) RolloutEvidenceFor(ctx context.Context, namespace, app string) rt.Evidence {
	return c.rolloutError(ctx, namespace, app, "").Evidence
}

// ListRouteNames returns the shared-address routes an app currently owns —
// the ones donkey renders from `routes:` in donkey.yaml.
func (c *Client) ListRouteNames(ctx context.Context, namespace, space, app string) ([]string, error) {
	routes, err := c.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s,%s=%s",
			manifests.LabelInstallation, c.installation(), manifests.LabelSpace, space,
			manifests.LabelApp, app, manifests.LabelComponent, manifests.ComponentPathRoute),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes.Items {
		out = append(out, r.Name)
	}
	return out, nil
}
