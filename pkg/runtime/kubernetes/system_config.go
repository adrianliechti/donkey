package kubernetes

import (
	"context"
	"errors"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// MigrateSystemConfig copies platform settings before the installer removes
// legacy resources. A new installation has no settings until first sign-in.
func (r *Runtime) MigrateSystemConfig(ctx context.Context) error {
	_, err := r.PlatformConfig(ctx)
	if errors.Is(err, rt.ErrPlatformNotFound) {
		return nil
	}
	return err
}

// SystemSecretData retains credentials when upgrading to the shared donkey
// resource name. An ownership or read error must never rotate session keys.
func (c *Client) SystemSecretData(ctx context.Context, namespace string) (map[string]string, error) {
	identity := manifests.SystemLabels(manifests.ServerName)
	data, err := c.GetOwnedSecretData(ctx, namespace, manifests.SystemConfigSecretName, identity)
	if err != nil || len(data) > 0 {
		return data, err
	}
	return c.GetOwnedSecretData(ctx, namespace, "donkey-system-config", identity)
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
	return c.DeleteObject(ctx, "ConfigMap", namespace, "donkey.platform", manifests.SystemLabels("donkey-platform"))
}
