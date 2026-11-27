package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	snapshot := rt.GatewaySnapshot{Spaces: policies, Platform: platform, Maintenance: map[string]config.SpaceMaintenance{}}
	enabled := platform.GatewayMaintenance != (config.GatewayMaintenance{})
	for _, cm := range cms {
		if cm.DeletionTimestamp != nil {
			continue
		}
		space := cm.Labels[manifests.LabelSpace]
		if raw := cm.Data[maintenanceKey]; enabled && raw != "" && config.ValidName(space) && cm.Name == manifests.SpaceMarkerName(space) &&
			g.Client.requireOwnedObject("ConfigMap", cm.Namespace, cm.Name, cm.Labels, manifests.SpaceLabels(space)) == nil {
			var state config.SpaceMaintenance
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				return rt.GatewaySnapshot{}, fmt.Errorf("reading maintenance state for %s: %w", space, err)
			}
			snapshot.Maintenance[space] = state
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

const maintenanceKey = "gateway-maintenance.json"

func (r *Runtime) SpaceMaintenance(ctx context.Context, space string) (config.SpaceMaintenance, error) {
	var state config.SpaceMaintenance
	cm, err := r.spaceConfigMap(ctx, space)
	if err == nil && cm.Data[maintenanceKey] != "" {
		err = json.Unmarshal([]byte(cm.Data[maintenanceKey]), &state)
	}
	return state, err
}

func (r *Runtime) SetSpaceMaintenance(ctx context.Context, space string, state config.SpaceMaintenance) error {
	cm, err := r.spaceConfigMap(ctx, space)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"uid": string(cm.UID)},
		"data":     map[string]string{maintenanceKey: string(raw)},
	})
	if err != nil {
		return err
	}
	_, err = r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-maintenance"})
	return err
}
