package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const spaceConfigKey = "config.yaml"

func (r *Runtime) EnsureSpaceConfigs(ctx context.Context) error {
	if err := r.ValidateExistingNames(ctx); err != nil {
		return err
	}
	if err := r.MigrateSystemConfig(ctx); err != nil {
		return err
	}
	if err := r.migrateSharedWorkloadNames(ctx); err != nil {
		return err
	}
	if err := r.migrateRouteConfigs(ctx); err != nil {
		return err
	}
	spaces, err := r.ListSpaces(ctx)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		if err := r.ensureSpaceHome(ctx, space); err != nil {
			return err
		}
		if err := r.migrateSpaceIsolation(ctx, space); err != nil {
			return err
		}
		if err := r.reconcilePluginEgress(ctx, space); err != nil {
			return err
		}
	}
	if err := r.migrateServiceSecrets(ctx); err != nil {
		return err
	}
	return r.migrateAppTrafficNames(ctx)
}

func (r *Runtime) ownedSpaceNamespace(ctx context.Context, space string) (*corev1.Namespace, error) {
	ns, err := r.c.Clientset.CoreV1().Namespaces().Get(ctx, r.ns(space), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, rt.ErrSpaceNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("Namespace", "", ns.Name, ns.Labels, manifests.SpaceLabels(space)); err != nil {
		return nil, err
	}
	if ns.Annotations[manifests.AnnotationManaged] != "true" {
		return nil, fmt.Errorf("the namespace is not a managed donkey space")
	}
	if ns.DeletionTimestamp != nil {
		return nil, fmt.Errorf("the space is being deleted")
	}
	return ns, nil
}

func (r *Runtime) spaceConfigMap(ctx context.Context, space string) (*corev1.ConfigMap, error) {
	if err := r.ValidateSpace(space); err != nil {
		return nil, err
	}
	if !r.opts.Placement.Shared() {
		if _, err := r.ownedSpaceNamespace(ctx, space); err != nil {
			return nil, err
		}
	}
	cm, err := r.c.Clientset.CoreV1().ConfigMaps(r.ns(space)).Get(ctx, manifests.SpaceMarkerName(space), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.migrateSpaceConfigMap(ctx, space)
	}
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SpaceLabels(space)); err != nil {
		return nil, err
	}
	if cm.DeletionTimestamp != nil {
		return nil, fmt.Errorf("the space is being deleted")
	}
	if _, current := cm.Data[spaceConfigKey]; !current {
		if _, oldKey := cm.Data["config.json"]; !oldKey {
			// A rendered app may have created an empty marker under the new
			// name. It must not hide membership stored under the old name.
			migrated, err := r.migrateSpaceConfigMap(ctx, space)
			if err == nil {
				return migrated, nil
			}
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
	}
	return cm, nil
}

func (r *Runtime) migrateSpaceConfigMap(ctx context.Context, space string) (*corev1.ConfigMap, error) {
	cms := r.c.Clientset.CoreV1().ConfigMaps(r.ns(space))
	old, err := cms.Get(ctx, "donkey.space."+space, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", old.Namespace, old.Name, old.Labels, manifests.SpaceLabels(space)); err != nil {
		return nil, err
	}
	settings := config.SpaceConfig{Domains: []string{}}
	if raw := old.Data["config.json"]; raw != "" {
		if err := config.DecodeYAML([]byte(raw), &settings); err != nil {
			return nil, fmt.Errorf("cannot migrate space settings: %w", err)
		}
		if err := settings.Validate(); err != nil {
			return nil, err
		}
	}
	raw, err := config.EncodeYAML(settings)
	if err != nil {
		return nil, err
	}
	fresh := manifests.SpaceMarker(space, old.Namespace)
	fresh.Labels = r.c.expectedObjectIdentity(fresh.Labels)
	fresh.Annotations = maps.Clone(old.Annotations)
	fresh.Data = maps.Clone(old.Data)
	if fresh.Data == nil {
		fresh.Data = map[string]string{}
	}
	fresh.Data[spaceConfigKey] = string(raw)
	delete(fresh.Data, "config.json")
	stored, err := cms.Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey-space-config"})
	if apierrors.IsAlreadyExists(err) {
		stored, err = cms.Get(ctx, fresh.Name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("ConfigMap", stored.Namespace, stored.Name, stored.Labels, manifests.SpaceLabels(space)); err != nil {
		return nil, err
	}
	if _, current := stored.Data[spaceConfigKey]; !current {
		if _, legacy := stored.Data["config.json"]; !legacy {
			if stored.Data == nil {
				stored.Data = map[string]string{}
			}
			stored.Data[spaceConfigKey] = string(raw)
			stored, err = cms.Update(ctx, stored, metav1.UpdateOptions{FieldManager: "donkey-space-config"})
			if err != nil {
				return nil, err
			}
		}
	}
	if err := r.c.deleteConfigMap(ctx, old); err != nil {
		return nil, err
	}
	return stored, nil
}

func (r *Runtime) SpaceConfig(ctx context.Context, space string) (config.SpaceConfig, error) {
	var settings config.SpaceConfig
	cm, err := r.spaceConfigMap(ctx, space)
	if apierrors.IsNotFound(err) {
		if r.opts.Placement.Shared() {
			return settings, rt.ErrSpaceNotFound
		}
		// A namespace from before space settings keeps its existing behavior.
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	return decodeSpaceConfig(cm)
}

func decodeSpaceConfig(cm *corev1.ConfigMap) (config.SpaceConfig, error) {
	var settings config.SpaceConfig
	raw, exists := cm.Data[spaceConfigKey]
	if !exists {
		raw = cm.Data["config.json"]
	}
	if raw != "" {
		if err := config.DecodeYAML([]byte(raw), &settings); err != nil {
			return settings, fmt.Errorf("cannot read space settings: %w", err)
		}
		if err := settings.Validate(); err != nil {
			return settings, err
		}
	}
	return settings, nil
}

var _ rt.SpaceConfigsProvider = (*Runtime)(nil)

// ListSpaceConfigs keeps fleet authorization fresh with one ConfigMap list
// and, for per-space placement, one namespace list. It shares no policy cache
// with the workload overview. Legacy or conflicting markers use the existing
// ownership-checked reader, so a bulk read cannot bypass migration or RBAC.
func (r *Runtime) ListSpaceConfigs(ctx context.Context) (map[string]config.SpaceConfig, error) {
	ns := r.opts.Placement.SharedNamespace // empty means NamespaceAll
	list, err := r.c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s", manifests.LabelInstallation, r.c.installation(),
			manifests.LabelRole, manifests.RoleSpace, manifests.LabelSpace),
	})
	if err != nil {
		return nil, err
	}
	markers := map[string]*corev1.ConfigMap{}
	spaces := map[string]bool{}
	for i := range list.Items {
		cm := &list.Items[i]
		space := cm.Labels[manifests.LabelSpace]
		if space == "" || cm.Namespace != r.ns(space) || cm.DeletionTimestamp != nil ||
			r.c.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SpaceLabels(space)) != nil {
			continue
		}
		if r.opts.Placement.Shared() {
			spaces[space] = true
		}
		if cm.Name == manifests.SpaceMarkerName(space) {
			markers[space] = cm
		}
	}
	if !r.opts.Placement.Shared() {
		refs, err := r.c.ListSpaces(ctx)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if ref.Namespace == r.ns(ref.Space) {
				spaces[ref.Space] = true
			}
		}
	}
	out := make(map[string]config.SpaceConfig, len(spaces))
	for space := range spaces {
		cm := markers[space]
		_, current := mapValue(cm, spaceConfigKey)
		_, legacy := mapValue(cm, "config.json")
		var settings config.SpaceConfig
		var err error
		if cm == nil || (!current && !legacy) {
			settings, err = r.SpaceConfig(ctx, space)
		} else {
			settings, err = decodeSpaceConfig(cm)
		}
		if err != nil {
			return nil, fmt.Errorf("reading space %s settings: %w", space, err)
		}
		out[space] = settings
	}
	return out, nil
}

func mapValue(cm *corev1.ConfigMap, key string) (string, bool) {
	if cm == nil {
		return "", false
	}
	value, found := cm.Data[key]
	return value, found
}

func (r *Runtime) createSpaceConfig(ctx context.Context, space string, settings config.SpaceConfig) error {
	raw, err := config.EncodeYAML(settings)
	if err != nil {
		return err
	}
	cm := manifests.SpaceMarker(space, r.ns(space))
	cm.Labels = r.c.expectedObjectIdentity(cm.Labels)
	cm.Data = map[string]string{spaceConfigKey: string(raw)}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm,
		metav1.CreateOptions{FieldManager: "donkey-space-config"})
	return err
}

func (r *Runtime) SetSpaceConfig(ctx context.Context, space string, settings config.SpaceConfig) error {
	if settings.Domains == nil {
		settings.Domains = []string{}
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	cm, err := r.spaceConfigMap(ctx, space)
	if apierrors.IsNotFound(err) && !r.opts.Placement.Shared() {
		// The namespace exists and is owned; add the config on its first edit.
		err = r.createSpaceConfig(ctx, space, settings)
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		cm, err = r.spaceConfigMap(ctx, space)
	}
	if apierrors.IsNotFound(err) {
		return rt.ErrSpaceNotFound
	}
	if err != nil {
		return err
	}
	settingsYAML, err := config.EncodeYAML(settings)
	if err != nil {
		return err
	}
	// Last save wins. Other ConfigMap keys and metadata are preserved.
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": cm.UID},
		"data":     map[string]any{spaceConfigKey: string(settingsYAML), "config.json": nil},
	})
	if err != nil {
		return err
	}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: "donkey-space-config"})
	return err
}

// Every space has a ConfigMap, in both placement modes. Initialize it once;
// deploy's ordinary apply does not own config.yaml and cannot reset it.
func (r *Runtime) ensureSpaceHome(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	settings := config.DefaultSpaceConfig(space, r.opts.Domain)
	if !r.opts.Placement.Shared() {
		_, err := r.ownedSpaceNamespace(ctx, space)
		if err == nil {
			settings = config.SpaceConfig{Domains: []string{}} // existing custom domains keep working
		} else if err == rt.ErrSpaceNotFound {
			ns := manifests.Namespace(space, r.ns(space))
			ns.Labels = r.c.expectedObjectIdentity(ns.Labels)
			_, err = r.c.Clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{FieldManager: "donkey"})
			if apierrors.IsAlreadyExists(err) {
				_, err = r.ownedSpaceNamespace(ctx, space)
			}
		}
		if err != nil {
			return err
		}
	}
	if cm, err := r.spaceConfigMap(ctx, space); err == nil {
		if cm.Data[spaceConfigKey] == "" {
			saved, err := r.SpaceConfig(ctx, space)
			if err != nil {
				return err
			}
			return r.SetSpaceConfig(ctx, space, saved)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	err := r.createSpaceConfig(ctx, space, settings)
	if apierrors.IsAlreadyExists(err) {
		_, err = r.spaceConfigMap(ctx, space)
	}
	return err
}

// ValidateExistingNames catches incompatible logical names before an upgrade
// mutates any resource. Spaces, apps and services are never silently renamed.
func (r *Runtime) ValidateExistingNames(ctx context.Context) error {
	spaces, err := r.ListSpaces(ctx)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		if err := r.ValidateSpace(space); err != nil {
			return fmt.Errorf("existing space must be moved before upgrading: %w", err)
		}
		services, err := r.ListServices(ctx, space, addons.SharedOwner)
		if err != nil {
			return err
		}
		for _, service := range services {
			if err := config.ValidateServiceName(service.Name); err != nil {
				return fmt.Errorf("existing service in space %q must be renamed before upgrading: %w", space, err)
			}
		}
	}
	apps, err := r.c.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if err := config.ValidateAppName(app.App); err != nil {
			return fmt.Errorf("existing app in space %q must be renamed before upgrading: %w", app.Space, err)
		}
	}
	return nil
}
