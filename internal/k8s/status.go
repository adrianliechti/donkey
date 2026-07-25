package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// AppStatus is a plain snapshot of an app's health for `donkey status`.
type AppStatus struct {
	Found        bool
	Image        string
	ReadyReplica int32
	WantReplica  int32
	Pods         []string
	IngressHost  string
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

	if pods, err := c.appPods(ctx, namespace, app); err == nil {
		for _, p := range pods.Items {
			st.Pods = append(st.Pods, podSummary(&p))
		}
	}
	routeGVR := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	if route, err := c.Dynamic.Resource(routeGVR).Namespace(namespace).Get(ctx, app, metav1.GetOptions{}); err == nil {
		if hosts, ok, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames"); ok && len(hosts) > 0 {
			st.IngressHost = hosts[0]
		}
	}
	return st, nil
}

// Healthy reports whether the app looks fully up.
func (s *AppStatus) Healthy() bool {
	return s.Found && s.ReadyReplica >= s.WantReplica
}

func (s *AppStatus) ReadyText() string {
	return fmt.Sprintf("%d of %d running", s.ReadyReplica, s.WantReplica)
}
