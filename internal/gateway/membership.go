package gateway

import (
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
)

func (r route) allows(subject, email string, groups []string) bool {
	if r.Space == "" && r.App == "" {
		// The console admits verified sign-ins; its API checks each action.
		return true
	}
	id := &identity.Identity{Subject: subject, Email: email, Groups: groups}
	return r.Platform.IsAdmin(id) || (r.Policy != nil && r.Policy.Role(id) != "")
}

func applyPolicy(r *route, policies map[string]config.SpaceConfig, platform config.PlatformConfig) {
	r.Platform = platform
	if r.Space == "" && r.App == "" {
		return
	}
	policy := policies[r.Space]
	r.Policy = &policy
	if !policy.AllowPublic {
		r.Login = true
	}
}

func gatewayPolicies(cms []corev1.ConfigMap) (map[string]config.SpaceConfig, config.PlatformConfig, error) {
	policies := map[string]config.SpaceConfig{}
	var platform config.PlatformConfig
	for _, cm := range cms {
		if cm.DeletionTimestamp != nil {
			continue
		}
		if cm.Labels[manifests.LabelRole] == manifests.RoleSpace {
			space := cm.Labels[manifests.LabelSpace]
			if !config.ValidName(space) || cm.Name != manifests.SpaceMarkerName(space) {
				continue
			}
			var policy config.SpaceConfig
			raw := cm.Data["config.yaml"]
			if raw == "" {
				raw = cm.Data["config.json"]
			}
			if raw != "" {
				if err := config.DecodeYAML([]byte(raw), &policy); err != nil {
					return nil, platform, fmt.Errorf("space %s membership: %w", space, err)
				}
				if err := policy.Validate(); err != nil {
					return nil, platform, err
				}
			}
			policies[space] = policy
		}
		if cm.Name == manifests.SystemConfigMapName && cm.Labels[manifests.LabelRole] == manifests.SystemRole(manifests.ServerName) {
			raw := cm.Data["platform.yaml"]
			if raw == "" {
				raw = cm.Data["platform.json"]
			}
			if raw != "" {
				if err := config.DecodeYAML([]byte(raw), &platform); err != nil {
					return nil, platform, err
				}
				if err := platform.Validate(); err != nil {
					return nil, platform, err
				}
			}
		}
	}
	return policies, platform, nil
}
