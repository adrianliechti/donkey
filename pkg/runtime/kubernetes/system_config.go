package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// MigrateSystemConfig copies platform settings before the installer removes
// legacy resources. A new installation has no settings until first sign-in.
func (r *Runtime) MigrateSystemConfig(ctx context.Context) error {
	if err := r.c.MigrateGatewaySecrets(ctx, r.opts.SystemNamespace); err != nil {
		return err
	}
	_, err := r.PlatformConfig(ctx)
	if errors.Is(err, rt.ErrPlatformNotFound) {
		return nil
	}
	return err
}

// MigrateGatewaySecrets runs before the gateway starts issuing certificates,
// including when an operator applies a rendered bundle instead of the CLI.
// The original cache stays available to gateway pods during their rollout.
func (c *Client) MigrateGatewaySecrets(ctx context.Context, namespace string) error {
	old, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, "donkey-gateway-certs", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Secret", namespace, old.Name, old.Labels, manifests.SystemLabels(manifests.GatewayAppName)); err != nil {
		return err
	}
	return c.copySecret(ctx, old, manifests.GatewayCertsSecret)
}

// SystemSecretData retains credentials when upgrading to the shared donkey
// resource name. An ownership or read error must never rotate session keys.
func (c *Client) SystemSecretData(ctx context.Context, namespace string) (map[string]string, error) {
	identity := manifests.SystemLabels(manifests.ServerName)
	data, err := c.GetOwnedSecretData(ctx, namespace, manifests.SystemConfigSecretName, identity)
	if err != nil || len(data) > 0 {
		return normalizeSystemSecret(data), err
	}
	data, err = c.GetOwnedSecretData(ctx, namespace, "donkey-system-config", identity)
	return normalizeSystemSecret(data), err
}

// Run only after the new manifests have rolled out: old pods may still read
// the previous Secret while the update is in progress.
func (c *Client) DeleteLegacySystemConfig(ctx context.Context, namespace string) error {
	identity := manifests.SystemLabels(manifests.ServerName)
	if err := c.DeleteObject(ctx, "ConfigMap", namespace, manifests.ServerName, identity); err != nil {
		return err
	}
	if err := c.DeleteObject(ctx, "Secret", namespace, "donkey-system-config", identity); err != nil {
		return err
	}
	if err := c.DeleteObject(ctx, "ConfigMap", namespace, manifests.BuildkitAppName, manifests.SystemLabels(manifests.BuildkitAppName)); err != nil {
		return err
	}
	for _, old := range []struct{ name, component string }{
		{"donkey-system-registry", manifests.RoleRegistryAuth},
		{"donkey-gateway-certs", manifests.GatewayAppName},
	} {
		if err := c.DeleteObject(ctx, "Secret", namespace, old.name, manifests.SystemLabels(old.component)); err != nil {
			return fmt.Errorf("removing %s: %w", old.name, err)
		}
	}
	if err := c.DeleteObject(ctx, "ConfigMap", namespace, "donkey.platform", manifests.SystemLabels("donkey-platform")); err != nil {
		return err
	}
	return c.deleteLegacySecretKeys(ctx, namespace)
}

// Previous manifests used stringData, whose resulting data keys may not be
// removed by server-side apply. Prune only known legacy keys after all new
// consumers have rolled out, preserving other entries and current values.
func (c *Client) deleteLegacySecretKeys(ctx context.Context, namespace string) error {
	secrets := c.Clientset.CoreV1().Secrets(namespace)
	secret, err := secrets.Get(ctx, manifests.SystemConfigSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Secret", namespace, secret.Name, secret.Labels, manifests.SystemLabels(manifests.ServerName)); err != nil {
		return err
	}
	data := map[string]any{}
	for current, legacy := range systemSecretKeys {
		if _, exists := secret.Data[legacy]; !exists {
			continue
		}
		if _, exists := secret.Data[current]; !exists {
			return fmt.Errorf("cannot remove legacy credential key %s before %s exists", legacy, current)
		}
		data[legacy] = nil
	}
	if len(data) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": secret.UID}, "data": data})
	if err != nil {
		return err
	}
	_, err = secrets.Patch(ctx, secret.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey"})
	return err
}

// Keys match the environment variables consumed by the control-plane pods.
// Presence, including an explicitly empty value, takes precedence over legacy.
func normalizeSystemSecret(data map[string]string) map[string]string {
	for current, legacy := range systemSecretKeys {
		if _, found := data[current]; !found {
			if value, found := data[legacy]; found {
				data[current] = value
			}
		}
	}
	return data
}

var systemSecretKeys = map[string]string{
	"DONKEY_REGISTRY_PASSWORD":    "registry-password",
	"OPENAI_API_KEY":              "openai-api-key",
	"DONKEY_OIDC_CLIENT_ID":       "oidc-client-id",
	"DONKEY_OIDC_CLIENT_SECRET":   "oidc-client-secret",
	"DONKEY_SESSION_KEY":          "session-key",
	"DONKEY_TELEMETRY_KEY":        "telemetry-key",
	"DONKEY_BUILD_ENV":            "build-env",
	"DONKEY_BUILDER_INSTRUCTIONS": "builder-instructions",
	"HTTP_PROXY":                  "http-proxy",
	"HTTPS_PROXY":                 "https-proxy",
	"DONKEY_PACKAGES_PASSWORD":    "packages-password",
}
