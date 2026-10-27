package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
)

type oidcRegistration struct {
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
}

func (s *Server) oidcClient(clientID string) (*oidcClient, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.oidcClients[clientID]
	return client, client != nil
}

func (state *oidcProviderState) currentClient(client *oidcClient) bool {
	return client.Version != "" && state.Clients[client.ID].Version == client.Version
}

// Reconcile only complete, successful route snapshots. A removed or changed
// registration gets a new durable version even when its old settings return;
// unchanged registrations survive gateway restarts and transient API outages.
func (s *Server) syncOIDCClients(ctx context.Context, table map[string][]route) (map[string]*oidcClient, error) {
	clients := map[string]*oidcClient{}
	for _, routes := range table {
		for _, rt := range routes {
			if rt.Path != "/" || rt.Mode == config.AppModeAPI || rt.OIDC == nil || !config.ValidName(rt.Space) || !config.ValidName(rt.App) {
				continue
			}
			if err := rt.OIDC.Validate(); err != nil {
				return nil, err
			}
			id := config.OIDCClientID(rt.Space, rt.App)
			if _, exists := clients[id]; exists {
				// Ambiguous clients cannot authorize against either route.
				clients[id] = nil
				continue
			}
			clients[id] = &oidcClient{ID: id, Route: rt}
		}
	}
	if s.oidcStore == nil && len(clients) == 0 {
		return clients, nil
	}
	err := s.updateOIDCState(ctx, func(state *oidcProviderState) error {
		registrations := make(map[string]oidcRegistration, len(clients))
		for id, client := range clients {
			if client == nil {
				continue
			}
			raw, _ := json.Marshal(client.Route.OIDC)
			fingerprint := oidcDigest(string(raw) + "\x00" + s.cfg.OIDCIssuer + "\x00" + s.cfg.OIDCClientID)
			registration := state.Clients[id]
			if registration.Version == "" || registration.Fingerprint != fingerprint {
				registration = oidcRegistration{Fingerprint: fingerprint, Version: randomHex(32)}
			}
			registrations[id] = registration
			client.Version = registration.Version
		}
		state.Clients = registrations
		for id, code := range state.Codes {
			if code.Request.Version != registrations[code.Request.ClientID].Version {
				delete(state.Codes, id)
			}
		}
		for id, grant := range state.Grants {
			if grant.Version != registrations[grant.ClientID].Version {
				delete(state.Grants, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("syncing OIDC registrations: %w", err)
	}
	return clients, nil
}
