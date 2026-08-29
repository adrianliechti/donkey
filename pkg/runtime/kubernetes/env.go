package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// GetSecretData returns a secret's data as strings; a missing secret is an
// empty map.
func (c *Client) GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error) {
	sec, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	return out, nil
}

// GetEnv returns the app's current env vars from its env secret.
func (c *Client) GetEnv(ctx context.Context, namespace, app string) (map[string]string, error) {
	return c.GetSecretData(ctx, namespace, manifests.EnvSecretName(app))
}

// SetEnv replaces the app's env secret with the given variables.
func (c *Client) SetEnv(ctx context.Context, namespace, space, app, objectName string, env map[string]string) error {
	data := map[string][]byte{}
	for k, v := range env {
		data[k] = []byte(v)
	}
	sec := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.EnvSecretName(objectName),
			Namespace: namespace,
			Labels:    manifests.Labels(space, app, manifests.ComponentConfig),
		},
		Data: data,
	}
	return c.Apply(ctx, []runtime.Object{sec})
}

// SetAddonEnvFrom adds or removes an addon's binding secret on the app
// Deployment's envFrom, so a change made in the console reaches the running
// app without a deploy (the template change triggers the rollout). Returns
// false when the app isn't deployed yet — the next deploy wires it instead.
func (c *Client) SetAddonEnvFrom(ctx context.Context, namespace, app, secret string, add bool) (bool, error) {
	deps := c.Clientset.AppsV1().Deployments(namespace)
	dep, err := deps.Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return false, nil
	}
	container := &dep.Spec.Template.Spec.Containers[0]
	idx := -1
	for i, e := range container.EnvFrom {
		if e.SecretRef != nil && e.SecretRef.Name == secret {
			idx = i
		}
	}
	if add == (idx >= 0) {
		return true, nil // already as wanted — no pointless rollout
	}
	if add {
		container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			},
		})
	} else {
		container.EnvFrom = append(container.EnvFrom[:idx], container.EnvFrom[idx+1:]...)
	}
	if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

// RestartApp triggers a fresh rollout so pods pick up changed env vars.
// Returns false when the app isn't deployed yet (nothing to restart).
func (c *Client) RestartApp(ctx context.Context, namespace, app string) (bool, error) {
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"donkey.dev/restarted-at":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339))
	_, err := c.Clientset.AppsV1().Deployments(namespace).Patch(ctx, app,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
