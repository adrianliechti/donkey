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
	return secretStringData(sec), nil
}

// GetOwnedSecretData reads a donkey-created Secret only after its complete
// immutable identity matches. GetSecretData remains for explicitly configured
// source Secrets that belong to the cluster operator, not donkey.
func (c *Client) GetOwnedSecretData(ctx context.Context, namespace, name string, identity map[string]string) (map[string]string, error) {
	sec, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject("Secret", namespace, name, sec.Labels, identity); err != nil {
		return nil, err
	}
	return secretStringData(sec), nil
}

func secretStringData(sec *corev1.Secret) map[string]string {
	out := map[string]string{}
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	return out
}

// GetEnv returns the app's current env vars from its env secret.
func (c *Client) GetEnv(ctx context.Context, namespace, space, app, objectName string) (map[string]string, error) {
	return c.GetOwnedSecretData(ctx, namespace, manifests.EnvSecretName(objectName),
		manifests.Labels(space, app, manifests.ComponentConfig))
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
func (c *Client) SetAddonEnvFrom(ctx context.Context, namespace, objectName, space, app, secret string, add bool) (bool, error) {
	deps := c.Clientset.AppsV1().Deployments(namespace)
	dep, err := deps.Get(ctx, objectName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.requireOwnedObject("Deployment", namespace, objectName, dep.Labels,
		manifests.Labels(space, app, manifests.ComponentWeb)); err != nil {
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
func (c *Client) RestartApp(ctx context.Context, namespace, objectName, space, app string) (bool, error) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, objectName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.requireOwnedObject("Deployment", namespace, objectName, dep.Labels,
		manifests.Labels(space, app, manifests.ComponentWeb)); err != nil {
		return false, err
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		manifests.AnnotationRestartedAt, time.Now().UTC().Format(time.RFC3339))
	_, err = c.Clientset.AppsV1().Deployments(namespace).Patch(ctx, objectName,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return false, err
	}
	return true, nil
}
