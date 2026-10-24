package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

// ReconcileAccess moves saved app users/groups into space viewers and removes
// the old lists. It also closes all public routes in spaces that disallow them.
// Membership is saved before old entries are removed, making retries safe.
// An empty target reconciles every space at server startup.
func ReconcileAccess(ctx context.Context, provider Provider, target string) error {
	apps, err := provider.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if target != "" && app.Space != target {
			continue
		}
		if err := reconcileAppAccess(ctx, provider, app); err != nil {
			return fmt.Errorf("reconciling access for %s/%s: %w", app.Space, app.App, err)
		}
	}
	return nil
}

func reconcileAppAccess(ctx context.Context, provider Provider, app AppRef) error {
	policy, err := provider.SpaceConfig(ctx, app.Space)
	if err != nil {
		return err
	}
	network, err := provider.Network(ctx, app.Space, app.App)
	if err != nil {
		return err
	}
	legacy := []*config.AccessConfig{network.Access}
	for _, route := range network.Routes {
		legacy = append(legacy, route.Access)
	}
	raw, err := provider.AppMeta(ctx, app.Space, app.App, MetaConsole)
	if err != nil {
		return err
	}
	var console map[string]json.RawMessage
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &console); err != nil {
			return err
		}
	}
	var saved config.AccessConfig
	if value, found := console["access"]; found {
		if err := json.Unmarshal(value, &saved); err != nil {
			return err
		}
		legacy = append(legacy, &saved)
	}
	changed := false
	for _, members := range legacy {
		if members == nil {
			continue
		}
		// Use the authorization rules for deduplication across all space roles:
		// emails ignore case; subject IDs and group names match exactly.
		for _, user := range members.Users {
			id := identity.Identity{Email: user}
			if subject, ok := strings.CutPrefix(user, "sub:"); ok {
				id = identity.Identity{Subject: subject}
			}
			if policy.Role(&id) == "" {
				policy.Viewers.Users = append(policy.Viewers.Users, user)
				changed = true
			}
		}
		for _, group := range members.Groups {
			if policy.Role(&identity.Identity{Groups: []string{group}}) == "" {
				policy.Viewers.Groups = append(policy.Viewers.Groups, group)
				changed = true
			}
		}
	}
	if changed {
		if err := provider.SetSpaceConfig(ctx, app.Space, policy); err != nil {
			return err
		}
	}
	if err := provider.ReconcileAppRoutes(ctx, app.Space, app.App, !policy.AllowPublic); err != nil {
		return err
	}
	if console != nil {
		_, dirty := console["access"]
		delete(console, "access")
		if !policy.AllowPublic && string(console["exposure"]) == `"public"` {
			console["exposure"] = json.RawMessage(`"protected"`)
			dirty = true
		}
		if dirty {
			value, err := json.Marshal(console)
			if err != nil {
				return err
			}
			text := string(value)
			if err := provider.SetAppMeta(ctx, app.Space, app.App, MetaConsole, &text); err != nil {
				return err
			}
		}
	}
	return nil
}
