package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/servicepolicy"
)

// ServiceSelection snapshots the service instances owned by one space.
// A service counts even if no app is connected to it. Previews can try several independent
// additions against the same snapshot. Mutations always read a fresh one.
type ServiceSelection struct {
	policy    *servicepolicy.Policy
	space     string
	existing  map[string]string
	offerings map[string]servicepolicy.Service
}

func ReadServiceSelection(ctx context.Context, r serviceAccessReader, policy *servicepolicy.Policy, space string) (*ServiceSelection, error) {
	if policy == nil {
		return nil, nil
	}
	s := &ServiceSelection{
		policy: policy, space: space,
		existing:  map[string]string{},
		offerings: map[string]servicepolicy.Service{},
	}
	existing, err := r.ListServices(ctx, space, addons.AllOwners)
	if err != nil {
		return nil, err
	}
	for _, b := range existing {
		id := config.NormalizeAddonType(b.Type)
		if saved := s.existing[b.Name]; saved != "" {
			if id != "" && id != saved {
				return nil, fmt.Errorf("conflicting offering IDs for service %s", b.Name)
			}
			continue
		}
		s.existing[b.Name] = id
	}
	return s, nil
}

func (s *ServiceSelection) Check(ctx context.Context, proposed []config.AddonConfig) error {
	if s == nil {
		return nil
	}
	selected := maps.Clone(s.existing)
	for _, choice := range proposed {
		id := config.NormalizeAddonType(choice.Type)
		if saved, ok := selected[choice.Name]; ok {
			if id != "" && id != saved {
				return UserError{Err: fmt.Errorf("service %s already uses offering %q", choice.Name, saved)}
			}
			id = saved
		}
		selected[choice.Name] = id
	}
	services := make([]servicepolicy.Service, 0, len(selected))
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		id := selected[name]
		if id == "" {
			return fmt.Errorf("service policy cannot identify the offering for %s", name)
		}
		service, ok := s.offerings[id]
		if !ok {
			a, err := addons.Get(ctx, id)
			if err != nil {
				return fmt.Errorf("resolving service policy metadata for %s: %w", name, err)
			}
			desc, err := a.Describe(ctx)
			if err != nil {
				return fmt.Errorf("resolving service policy metadata for %s: %w", name, err)
			}
			service = s.policy.Offering(desc)
			s.offerings[id] = service
		}
		service.Name = name
		services = append(services, service)
	}
	denied, err := s.policy.Decision(ctx, s.space, services)
	if denied != "" {
		return UserError{Err: fmt.Errorf("service policy: %s", denied)}
	}
	return err
}

func CheckServiceSelection(ctx context.Context, r serviceAccessReader, policy *servicepolicy.Policy, space string, proposed []config.AddonConfig) error {
	selection, err := ReadServiceSelection(ctx, r, policy, space)
	if err != nil {
		return err
	}
	return selection.Check(ctx, proposed)
}
