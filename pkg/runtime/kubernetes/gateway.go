package kubernetes

import (
	"context"
	"fmt"
	"net/url"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
)

// GatewayAdapter confines all edge access to this installation's runtime.
type GatewayAdapter struct {
	Client    *Client
	Namespace string
}

func (g *GatewayAdapter) GatewaySnapshot(ctx context.Context) (rt.GatewaySnapshot, error) {
	cms, err := g.Client.ListGatewayConfigMaps(ctx)
	if err != nil {
		return rt.GatewaySnapshot{}, err
	}
	policies, platform, err := gatewayPolicies(cms)
	if err != nil {
		return rt.GatewaySnapshot{}, err
	}
	snapshot := rt.GatewaySnapshot{Spaces: policies, Platform: platform}
	for _, cm := range cms {
		if cm.DeletionTimestamp != nil {
			continue
		}
		if record, ok := routeFromConfigMap(cm); ok {
			snapshot.Routes = append(snapshot.Routes, record)
		}
	}
	return snapshot, nil
}

func routeFromConfigMap(cm corev1.ConfigMap) (config.GatewayRoute, bool) {
	settings, err := manifests.ReadRoute(&cm)
	if err != nil || settings.Host == "" || settings.Backend == "" {
		return config.GatewayRoute{}, false
	}
	target, err := url.Parse(settings.Backend)
	if err != nil || target.Host == "" || target.Scheme != "http" {
		return config.GatewayRoute{}, false
	}
	// Identity comes from ownership labels, never editable routing fields.
	space, app := cm.Labels[manifests.LabelSpace], cm.Labels[manifests.LabelApp]
	if machine := cm.Labels[manifests.LabelMachine]; machine != "" || settings.Machine != "" {
		// A machine's route is nobody's app: it names its machine in a label
		// that has to agree with the record, and admits one owner.
		if !config.ValidMachineID(machine) || settings.Machine != machine || settings.Owner == "" ||
			space != "" || app != "" || cm.Labels[manifests.LabelRole] != manifests.RoleMachineRoute {
			return config.GatewayRoute{}, false
		}
		return config.MachineRoute(machine, settings.Owner, settings.Host, settings.Backend), true
	}
	systemRoute := space == "" && app == "" &&
		cm.Labels[manifests.LabelRole] == manifests.SystemRole(manifests.ServerName) && settings.Token
	if (!config.ValidName(space) || !config.ValidName(app)) && !systemRoute {
		return config.GatewayRoute{}, false
	}
	settings.Space, settings.App = space, app
	settings.Token = systemRoute
	return settings, true
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
