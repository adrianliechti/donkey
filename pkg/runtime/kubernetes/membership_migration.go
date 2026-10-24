package kubernetes

import (
	"context"
	"fmt"
	"log"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Remove obsolete grants before the current strict decoders read saved policy.
// This never changes users/groups or touches another installation's records.
func (r *Runtime) migrateLegacyRoles(ctx context.Context) error {
	cms, err := r.c.ListGatewayConfigMaps(ctx)
	if err != nil {
		return err
	}
	for _, cm := range cms {
		changed := false
		var warnings []string
		strip := func(values map[string]string, key string, fields ...string) error {
			original, found := values[key]
			if !found || original == "" {
				return nil
			}
			raw, removed, err := config.RemoveLegacyRoles([]byte(original), fields...)
			if err != nil {
				return fmt.Errorf("ConfigMap %s/%s %s: %w", cm.Namespace, cm.Name, key, err)
			}
			if raw != nil {
				values[key] = string(raw)
				changed = true
			}
			for _, role := range removed {
				warnings = append(warnings, key+" "+role)
			}
			return nil
		}
		if manifests.IsRoute(&cm) {
			if _, current := cm.Data[manifests.RouteAccessKey]; current {
				if err := strip(cm.Data, manifests.RouteAccessKey, ""); err != nil {
					return err
				}
			} else if _, current := cm.Data[manifests.RouteConfigKey]; !current {
				if err := strip(cm.Annotations, manifests.AnnotationAccess, ""); err != nil {
					return err
				}
			}
		}
		// Match the readers' precedence; stale fallback records are inactive.
		key := func(current, legacy string) string {
			if _, found := cm.Data[current]; found {
				return current
			}
			return legacy
		}
		if cm.Labels[manifests.LabelRole] == manifests.RoleSpace {
			if err := strip(cm.Data, key(spaceConfigKey, "config.json"), "admins", "developers", "viewers"); err != nil {
				return err
			}
		}
		if cm.Namespace == r.opts.SystemNamespace {
			if err := strip(cm.Data, key(platformConfigKey, "platform.json"), "admins"); err != nil {
				return err
			}
			if cm.Name == "donkey.platform" {
				if err := strip(cm.Data, "config.json", "admins"); err != nil {
					return err
				}
			}
		}
		if !changed {
			continue
		}
		if _, err := r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, &cm, metav1.UpdateOptions{FieldManager: "donkey-membership-migration"}); err != nil {
			return err
		}
		if len(warnings) > 0 {
			log.Printf("WARNING: removed unsupported legacy role grants from ConfigMap %s/%s: %q; assign users/groups explicitly to restore access", cm.Namespace, cm.Name, warnings)
		}
	}
	return nil
}
