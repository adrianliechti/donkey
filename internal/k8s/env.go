package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
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
func (c *Client) SetEnv(ctx context.Context, namespace, space, app string, env map[string]string) error {
	data := map[string][]byte{}
	for k, v := range env {
		data[k] = []byte(v)
	}
	sec := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.EnvSecretName(app),
			Namespace: namespace,
			Labels:    manifests.Labels(space, app, "env"),
		},
		Data: data,
	}
	return c.Apply(ctx, []runtime.Object{sec})
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
