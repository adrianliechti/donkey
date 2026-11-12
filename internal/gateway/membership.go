package gateway

import (
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
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
