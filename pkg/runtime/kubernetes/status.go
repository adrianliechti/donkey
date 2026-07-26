package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	st.Image = dep.Annotations["donkey.dev/image"]
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

	if pods, err := c.appPods(ctx, namespace, app); err == nil {
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

// DeploymentComponent reports whether a Deployment of this name exists, and
// which donkey component it belongs to ("" for an unlabeled one).
func (c *Client) DeploymentComponent(ctx context.Context, namespace, name string) (string, bool, error) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return dep.Labels[manifests.LabelComponent], true, nil
}

// ListAddonBindings finds the app's addon binding Secrets.
func (c *Client) ListAddonBindings(ctx context.Context, namespace, app string) ([]rt.ServiceBindingInfo, error) {
	secrets, err := c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + app,
	})
	if err != nil {
		return nil, err
	}
	var out []rt.ServiceBindingInfo
	for _, sec := range secrets.Items {
		component := sec.Labels[manifests.LabelComponent]
		if !strings.HasPrefix(component, "addon-") {
			continue
		}
		b := rt.ServiceBindingInfo{Name: strings.TrimPrefix(component, "addon-")}
		for k := range sec.Data {
			b.Keys = append(b.Keys, k)
		}
		sort.Strings(b.Keys)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SharedBindingValues collects one key's values across every space's shared
// binding Secrets — the AI proxy's token list. owner is the pseudo-app the
// shared bindings belong to.
func (c *Client) SharedBindingValues(ctx context.Context, owner, key string) ([]string, error) {
	secrets, err := c.Clientset.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + owner,
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sec := range secrets.Items {
		if !strings.HasPrefix(sec.Labels[manifests.LabelComponent], "addon-") {
			continue
		}
		if v, ok := sec.Data[key]; ok && len(v) > 0 {
			out = append(out, string(v))
		}
	}
	return out, nil
}

// ListAddonAttachments names the apps holding a copy of the shared addon's
// binding (the component carries the addon- prefix already).
func (c *Client) ListAddonAttachments(ctx context.Context, namespace, component string) ([]string, error) {
	secrets, err := c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelComponent + "=" + component + "," + manifests.LabelShared + "=true",
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sec := range secrets.Items {
		if app := sec.Labels[manifests.LabelApp]; app != "" {
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
func (c *Client) ListRouteNames(ctx context.Context, namespace, app string) ([]string, error) {
	routes, err := c.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + app + "," + manifests.LabelComponent + "=" + manifests.ComponentRoute,
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
