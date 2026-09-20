package server

import (
	"context"
	"github.com/adrianliechti/donkey/internal/addons"
)

func (s *Server) serviceMetadata(ctx context.Context, space, name string, binding map[string]string) (addons.Metadata, error) {
	services, err := s.rt.ListServices(ctx, space, addons.SharedOwner)
	if err != nil {
		return addons.Metadata{}, err
	}
	metadata := addons.Metadata{Protocol: addons.LegacyProtocol(binding)}
	for _, service := range services {
		if service.Name == name {
			metadata.Type = service.Type
			if service.Protocol != "" {
				metadata.Protocol = addons.NormalizeProtocol(service.Protocol)
			}
			break
		}
	}
	return metadata, nil
}
