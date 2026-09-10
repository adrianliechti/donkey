package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const platformConfigName = "donkey.platform"

func (r *Runtime) platformConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := r.c.Clientset.CoreV1().ConfigMaps(r.opts.SystemNamespace).Get(ctx, platformConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, rt.ErrPlatformNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SystemLabels("donkey-platform")); err != nil {
		return nil, err
	}
	return cm, nil
}

func (r *Runtime) PlatformConfig(ctx context.Context) (config.PlatformConfig, error) {
	var settings config.PlatformConfig
	cm, err := r.platformConfigMap(ctx)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(cm.Data[spaceConfigKey]), &settings); err != nil {
		return settings, fmt.Errorf("cannot read platform settings: %w", err)
	}
	if settings.Admins.Empty() {
		return settings, fmt.Errorf("platform settings must contain an admin")
	}
	return settings, settings.Validate()
}

// Creation is atomic: simultaneous first sign-ins all use the one saved list.
// Subsequent saves deliberately use last-write-wins semantics.
func (r *Runtime) InitPlatformConfig(ctx context.Context, initial config.PlatformConfig) (config.PlatformConfig, error) {
	if err := initial.Validate(); err != nil {
		return config.PlatformConfig{}, err
	}
	if initial.Admins.Empty() {
		return config.PlatformConfig{}, fmt.Errorf("platform settings must contain an admin")
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		return config.PlatformConfig{}, err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: platformConfigName, Namespace: r.opts.SystemNamespace,
			Labels: r.c.expectedObjectIdentity(manifests.SystemLabels("donkey-platform")),
		},
		Data: map[string]string{spaceConfigKey: string(raw)},
	}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{FieldManager: "donkey-platform-config"})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return config.PlatformConfig{}, err
	}
	return r.PlatformConfig(ctx)
}

func (r *Runtime) SetPlatformConfig(ctx context.Context, settings config.PlatformConfig) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	if settings.Admins.Empty() {
		return fmt.Errorf("platform settings must contain an admin")
	}
	cm, err := r.platformConfigMap(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": cm.UID},
		"data":     map[string]string{spaceConfigKey: string(raw)},
	})
	if err != nil {
		return err
	}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-platform-config"})
	return err
}
