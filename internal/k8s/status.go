package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// AppStatus is a plain snapshot of an app's health for `donkey status`.
type AppStatus struct {
	Found        bool
	Image        string
	ReadyReplica int32
	WantReplica  int32
	// UpToDate reports that the observed state belongs to the latest
	// generation — without it, an old healthy pod would make a failing
	// rollout look green.
	UpToDate    bool
	Pods        []string
	IngressHost string
	// Restarts sums container restarts across the app's pods — the number
	// that separates "quietly struggling" from "running".
	Restarts int32
	// StartedAt is when the newest pod came up ("deployed X ago").
	StartedAt time.Time
}

func (c *Client) AppStatus(ctx context.Context, namespace, app string) (*AppStatus, error) {
	st := &AppStatus{}
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
			st.Pods = append(st.Pods, podSummary(&p))
			for _, cs := range p.Status.ContainerStatuses {
				st.Restarts += cs.RestartCount
			}
			if p.CreationTimestamp.Time.After(st.StartedAt) {
				st.StartedAt = p.CreationTimestamp.Time
			}
		}
	}
	if route, err := c.Dynamic.Resource(gvrFor["HTTPRoute"]).Namespace(namespace).Get(ctx, app, metav1.GetOptions{}); err == nil {
		if hosts, ok, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames"); ok && len(hosts) > 0 {
			st.IngressHost = hosts[0]
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

// Healthy reports whether the app's latest version is fully up.
func (s *AppStatus) Healthy() bool {
	return s.Found && s.UpToDate && s.ReadyReplica >= s.WantReplica
}

func (s *AppStatus) ReadyText() string {
	return fmt.Sprintf("%d of %d running", s.ReadyReplica, s.WantReplica)
}

// AddonBinding describes one attached service, by its binding Secret.
type AddonBinding struct {
	// Name is the addon's name (the component without the addon- prefix).
	Name string
	// Keys are the env var names the binding injects — never the values.
	Keys []string
}

// ListAddonBindings finds the app's addon binding Secrets.
func (c *Client) ListAddonBindings(ctx context.Context, namespace, app string) ([]AddonBinding, error) {
	secrets, err := c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + app,
	})
	if err != nil {
		return nil, err
	}
	var out []AddonBinding
	for _, sec := range secrets.Items {
		component := sec.Labels[manifests.LabelComponent]
		if !strings.HasPrefix(component, "addon-") {
			continue
		}
		b := AddonBinding{Name: strings.TrimPrefix(component, "addon-")}
		for k := range sec.Data {
			b.Keys = append(b.Keys, k)
		}
		sort.Strings(b.Keys)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RolloutEvidenceFor collects the same pod summaries, problem-pod logs and
// warning events a failed rollout reports — for on-demand diagnosis of an
// app that is currently unhealthy.
func (c *Client) RolloutEvidenceFor(ctx context.Context, namespace, app string) RolloutEvidence {
	return c.rolloutError(ctx, namespace, app, "").Evidence
}
