package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/servicepolicy"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// A choice creates a space service or connects it to App. Descriptor is set
// only by the provisioner, so the decision and creation use the same catalog read.
type ServiceChoice struct {
	Name       string            `json:"name"`
	Type       string            `json:"type,omitempty"`
	App        string            `json:"app,omitempty"`
	Descriptor *addonv1.Offering `json:"-"`
}

func AppServiceChoices(app *config.AppConfig) []ServiceChoice {
	choices := make([]ServiceChoice, 0, len(app.Addons))
	for _, a := range app.Addons {
		choices = append(choices, ServiceChoice{Name: a.Name, Type: a.Type, App: app.Name})
	}
	return choices
}

type ServiceSelection struct {
	policy   *servicepolicy.Policy
	space    string
	existing map[string]ServiceBindingInfo
	owners   map[[2]string]bool
	catalog  map[string]*addonv1.Offering
}

func ReadServiceSelection(ctx context.Context, r serviceAccessReader, policy *servicepolicy.Policy, space string) (*ServiceSelection, error) {
	if policy == nil {
		return nil, nil
	}
	bindings, err := r.ListServices(ctx, space, addons.AllOwners)
	if err != nil {
		return nil, err
	}
	s := &ServiceSelection{policy: policy, space: space, existing: map[string]ServiceBindingInfo{},
		owners: map[[2]string]bool{}, catalog: map[string]*addonv1.Offering{}}
	for _, b := range bindings {
		s.owners[[2]string{b.Owner, b.Name}] = true
		b.Type = config.NormalizeAddonType(b.Type)
		saved := s.existing[b.Name]
		if saved.Type != "" && b.Type != "" && b.Type != saved.Type {
			return nil, fmt.Errorf("conflicting offering IDs for service %s", b.Name)
		}
		if saved.Type == "" || b.Offering != nil {
			s.existing[b.Name] = b
		}
	}
	return s, nil
}

// CurrentServicePolicy assesses everything already present, independently of admission.
func CurrentServicePolicy(ctx context.Context, r serviceAccessReader, policy *servicepolicy.Policy, space string) (string, error) {
	s, err := ReadServiceSelection(ctx, r, policy, space)
	if err != nil || s == nil {
		return "", err
	}
	return s.decision(ctx, s.existing)
}

func (s *ServiceSelection) Check(ctx context.Context, proposed []ServiceChoice) error {
	if s == nil {
		return nil
	}
	selected := maps.Clone(s.existing)
	changed := false
	for _, choice := range proposed {
		id := config.NormalizeAddonType(choice.Type)
		b, exists := selected[choice.Name]
		if exists && b.Type != "" && id != "" && id != b.Type {
			return UserError{Err: fmt.Errorf("service %s already uses offering %q", choice.Name, b.Type)}
		}
		if !exists {
			b = ServiceBindingInfo{Type: id, Offering: choice.Descriptor}
		}
		selected[choice.Name] = b
		// Existing app connections can be reused after a policy change.
		// Explicit provisioning may change parameters or an external endpoint.
		if choice.App == "" || !s.owners[[2]string{choice.App, choice.Name}] {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	reason, err := s.decision(ctx, selected)
	if reason != "" {
		return UserError{Err: fmt.Errorf("service policy: %s", reason)}
	}
	return err
}

func (s *ServiceSelection) decision(ctx context.Context, selected map[string]ServiceBindingInfo) (string, error) {
	services := make([]servicepolicy.Service, 0, len(selected))
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		b := selected[name]
		if b.Type == "" {
			return "", fmt.Errorf("service policy cannot identify the offering for %s", name)
		}
		desc := b.Offering
		if desc == nil {
			desc = s.catalog[b.Type]
		}
		if desc == nil {
			var err error
			desc, err = addons.Describe(ctx, b.Type)
			if err != nil {
				return "", fmt.Errorf("resolving service policy metadata for %s: %w", name, err)
			}
			s.catalog[b.Type] = desc
		}
		if desc.Id != b.Type {
			return "", fmt.Errorf("service %s has mismatched offering metadata", name)
		}
		service := s.policy.Offering(desc)
		service.Name = name
		services = append(services, service)
	}
	return s.policy.Decision(ctx, s.space, services)
}

func CheckServiceSelection(ctx context.Context, r serviceAccessReader, policy *servicepolicy.Policy, space string, proposed []ServiceChoice) error {
	if policy == nil || len(proposed) == 0 {
		return nil
	}
	s, err := ReadServiceSelection(ctx, r, policy, space)
	if err != nil {
		return err
	}
	return s.Check(ctx, proposed)
}
