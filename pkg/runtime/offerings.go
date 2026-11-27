package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// ResolveServiceConnection rejects retired platform endpoints. Only an OpenAI
// endpoint recorded by a plugin during provisioning may receive the viewer's token.
func ResolveServiceConnection(m addons.Metadata, binding map[string]string, aiProxyURL string, provisioning func() (*addons.Provisioning, error)) (ServiceConnection, error) {
	c := ServiceConnection{Type: m.Type, Protocol: m.Protocol, Parameters: m.Parameters, Binding: binding}
	endpoint := binding["OPENAI_BASE_URL"]
	if (m.Protocol == addons.ProtocolOpenAI && endpoint == aiProxyURL) ||
		(m.Protocol == addons.ProtocolExa && binding["EXA_BASE_URL"] == strings.TrimSuffix(aiProxyURL, "/v1")+"/exa") {
		return ServiceConnection{}, UserError{Err: fmt.Errorf("the built-in %s service was removed; replace this binding with a plugin-provided service", m.Protocol)}
	}
	if endpoint == "" || m.Protocol != addons.ProtocolOpenAI {
		return c, nil
	}
	state, err := provisioning()
	if err != nil {
		return ServiceConnection{}, err
	}
	c.ManagedOpenAI = state != nil && state.Info.GetEnv()["OPENAI_BASE_URL"] == endpoint
	return c, nil
}

func OfferingPolicy(ctx context.Context, r spaceConfigReader, space string) (config.SpaceConfig, error) {
	policy, err := r.SpaceConfig(ctx, space)
	if errors.Is(err, ErrSpaceNotFound) {
		return config.SpaceConfig{}, nil
	}
	return policy, err
}

// PrepareService applies space defaults only at creation. Saved values win
// on subsequent requests, and existing instances retain their lifecycle even
// when their offering is removed from the space's catalog.
func PrepareService(ctx context.Context, r spaceConfigReader, common config.CommonOfferings, space string, desc *addonv1.Offering, given map[string]any, existing bool) (map[string]any, error) {
	if existing {
		return given, nil
	}
	policy, err := OfferingPolicy(ctx, r, space)
	if err != nil {
		return nil, err
	}
	if !policy.OfferingAllowed(common, desc.Id) {
		return nil, UserError{Err: fmt.Errorf("offering %s is not enabled in space %s; ask a space admin to add it", desc.Id, space)}
	}
	entry, _ := policy.Offering(desc.Id)
	if _, err := addons.ResolveParameters(desc.SpaceConfiguration, entry.Config); err != nil {
		return nil, UserError{Err: fmt.Errorf("offering %s needs configuration from a space admin", desc.Id)}
	}
	merged := map[string]any{}
	maps.Copy(merged, entry.Defaults)
	maps.Copy(merged, given)
	return merged, nil
}

func SpaceConfiguration(ctx context.Context, r spaceConfigReader, space string, desc *addonv1.Offering, previous *addons.Provisioning) (addons.Parameters, error) {
	policy, err := OfferingPolicy(ctx, r, space)
	if err != nil {
		return nil, err
	}
	entry, configured := policy.Offering(desc.Id)
	params, err := addons.PlanSpaceConfiguration(desc, entry.Config, configured, previous)
	if err != nil {
		return nil, UserError{Err: err}
	}
	return params, nil
}

type serviceAccessReader interface {
	spaceConfigReader
	ServiceBinding(context.Context, string, string, string) (map[string]string, error)
	ServiceConnection(context.Context, string, string) (ServiceConnection, error)
	ListServices(context.Context, string, string) ([]ServiceBindingInfo, error)
}

// CheckServiceAttachment allows current attachments to keep running but
// prevents new consumers from bypassing the space catalog through a binding.
func CheckServiceAttachment(ctx context.Context, r serviceAccessReader, common config.CommonOfferings, space, app, name string) error {
	if len(common) == 0 {
		return nil
	}
	attached, err := r.ServiceBinding(ctx, space, app, name)
	if err != nil || len(attached) > 0 {
		return err
	}
	connection, err := r.ServiceConnection(ctx, space, name)
	if err != nil {
		return err
	}
	if connection.Type == "" {
		// Failed and retained plugin instances have a lifecycle record even
		// while their binding is absent. They must not admit new consumers.
		services, err := r.ListServices(ctx, space, addons.SharedOwner)
		if err != nil {
			return err
		}
		for _, service := range services {
			if service.Name == name {
				connection.Type = service.Type
				break
			}
		}
		if connection.Type == "" {
			return nil // EnsureService checks creation.
		}
	}
	policy, err := OfferingPolicy(ctx, r, space)
	if err != nil {
		return err
	}
	if !policy.OfferingAllowed(common, connection.Type) {
		return UserError{Err: fmt.Errorf("offering %s is not enabled in space %s; ask a space admin to add it", connection.Type, space)}
	}
	return nil
}

// IncludePluginServices joins binding copies with the descriptor that actually
// provisioned the instance. Retained and pending instances also count.
func IncludePluginServices(bindings []ServiceBindingInfo, records []*addons.Provisioning) ([]ServiceBindingInfo, error) {
	byName := make(map[string]*addons.Provisioning, len(records))
	for _, record := range records {
		byName[record.Instance.Name] = record
	}
	bound := make(map[string]bool, len(records))
	for i := range bindings {
		b := &bindings[i]
		if p := byName[b.Name]; p != nil {
			if b.Type != "" && b.Type != p.Type {
				return nil, fmt.Errorf("conflicting offering IDs for service %s", b.Name)
			}
			b.Type, b.Offering = p.Type, p.Offering
			bound[b.Name] = true
		}
	}
	for name, p := range byName {
		if !bound[name] {
			bindings = append(bindings, ServiceBindingInfo{Name: name, Type: p.Type, Protocol: p.Protocol,
				Offering: p.Offering, Parameters: p.Parameters, State: p.State(), Message: p.Error})
		}
	}
	slices.SortFunc(bindings, func(a, b ServiceBindingInfo) int { return cmp.Compare(a.Name, b.Name) })
	return bindings, nil
}
