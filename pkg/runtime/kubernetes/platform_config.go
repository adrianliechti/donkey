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
	"k8s.io/client-go/util/retry"
)

const platformConfigKey = "platform.json"

func (r *Runtime) platformConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := r.c.Clientset.CoreV1().ConfigMaps(r.opts.SystemNamespace).Get(ctx, manifests.SystemConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SystemLabels(manifests.ServerName)); err != nil {
		return nil, err
	}
	return cm, nil
}

func (r *Runtime) PlatformConfig(ctx context.Context) (config.PlatformConfig, error) {
	var settings config.PlatformConfig
	cm, err := r.platformConfigMap(ctx)
	if apierrors.IsNotFound(err) {
		return r.migratePlatformConfig(ctx)
	}
	if err != nil {
		return settings, err
	}
	if _, exists := cm.Data[platformConfigKey]; !exists {
		return r.migratePlatformConfig(ctx)
	}
	return decodePlatformConfig(cm.Data[platformConfigKey])
}

// Preserve the admin list from installations that used a separate ConfigMap.
// InitPlatformConfig adds only platform.json and preserves the console route.
func (r *Runtime) migratePlatformConfig(ctx context.Context) (config.PlatformConfig, error) {
	cm, err := r.c.Clientset.CoreV1().ConfigMaps(r.opts.SystemNamespace).Get(ctx, "donkey.platform", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return config.PlatformConfig{}, rt.ErrPlatformNotFound
	}
	if err != nil {
		return config.PlatformConfig{}, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SystemLabels("donkey-platform")); err != nil {
		return config.PlatformConfig{}, err
	}
	settings, err := decodePlatformConfig(cm.Data["config.json"])
	if err != nil {
		return settings, err
	}
	return r.InitPlatformConfig(ctx, settings)
}

func decodePlatformConfig(raw string) (config.PlatformConfig, error) {
	var settings config.PlatformConfig
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
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
	err = retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		cm, err := r.platformConfigMap(ctx)
		if apierrors.IsNotFound(err) {
			_, err = r.c.Clientset.CoreV1().ConfigMaps(r.opts.SystemNamespace).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: manifests.SystemConfigMapName, Namespace: r.opts.SystemNamespace,
					Labels: r.c.expectedObjectIdentity(manifests.SystemLabels(manifests.ServerName))},
				Data: map[string]string{platformConfigKey: string(raw)},
			}, metav1.CreateOptions{FieldManager: "donkey-platform-config"})
			return err
		}
		if err != nil {
			return err
		}
		if _, exists := cm.Data[platformConfigKey]; exists {
			return nil
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[platformConfigKey] = string(raw)
		// Only first-user registration needs a conditional write. Settings
		// edits below stay last-save-wins, with no revision in the API.
		_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "donkey-platform-config"})
		return err
	})
	if err != nil {
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
		"data":     map[string]string{platformConfigKey: string(raw)},
	})
	if err != nil {
		return err
	}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-platform-config"})
	return err
}
