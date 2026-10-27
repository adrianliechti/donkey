package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type oidcProviderState struct {
	Clients  map[string]oidcRegistration `json:"clients,omitempty"`
	Keys     []oidcSigningKey            `json:"keys,omitempty"`
	Codes    map[string]oidcCode         `json:"codes,omitempty"`
	Grants   map[string]oidcGrant        `json:"grants,omitempty"`
	Consents map[string]int64            `json:"consents,omitempty"`
}

func mutateOIDCState(raw []byte, fn func(*oidcProviderState) error) ([]byte, error) {
	var state oidcProviderState
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, fmt.Errorf("reading OIDC state: %w", err)
		}
	}
	if state.Codes == nil {
		state.Codes = map[string]oidcCode{}
	}
	if state.Grants == nil {
		state.Grants = map[string]oidcGrant{}
	}
	if state.Consents == nil {
		state.Consents = map[string]int64{}
	}
	now := time.Now().Unix()
	for id, expires := range state.Consents {
		if expires <= now {
			delete(state.Consents, id)
		}
	}
	for id, code := range state.Codes {
		if code.Expires <= now {
			delete(state.Codes, id)
		}
	}
	for id, grant := range state.Grants {
		if grant.Expires <= now {
			delete(state.Grants, id)
		}
	}
	if err := fn(&state); err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// The protocol owns the schema; storage owns the transaction and durability.
func (s *Server) updateOIDCState(ctx context.Context, fn func(*oidcProviderState) error) error {
	if s.oidcStore == nil {
		return fmt.Errorf("OIDC storage is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.oidcStore.Update(ctx, func(raw []byte) ([]byte, error) { return mutateOIDCState(raw, fn) })
}
