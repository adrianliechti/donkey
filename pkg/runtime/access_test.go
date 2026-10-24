package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

type accessRuntime struct {
	Provider
	policy                    config.SpaceConfig
	network                   NetworkState
	console                   string
	policyWrites, routeWrites int
	writeError                error
	routeError, consoleError  error
}

func (r *accessRuntime) ListApps(context.Context) ([]AppRef, error) {
	return []AppRef{{Space: "team", App: "web"}}, nil
}
func (r *accessRuntime) SpaceConfig(context.Context, string) (config.SpaceConfig, error) {
	return r.policy, nil
}
func (r *accessRuntime) SetSpaceConfig(_ context.Context, _ string, policy config.SpaceConfig) error {
	if r.writeError != nil {
		return r.writeError
	}
	r.policy = policy
	r.policyWrites++
	return nil
}
func (r *accessRuntime) Network(context.Context, string, string) (*NetworkState, error) {
	return &r.network, nil
}
func (r *accessRuntime) AppMeta(context.Context, string, string, string) (string, error) {
	return r.console, nil
}
func (r *accessRuntime) SetAppMeta(_ context.Context, _, _, _ string, value *string) error {
	if r.consoleError != nil {
		return r.consoleError
	}
	r.console = *value
	return nil
}
func (r *accessRuntime) ReconcileAppRoutes(_ context.Context, _, _ string, protect bool) error {
	r.routeWrites++
	if r.routeError != nil {
		return r.routeError
	}
	r.network.Access = nil
	if protect {
		r.network.Protected = true
	}
	for i := range r.network.Routes {
		r.network.Routes[i].Access = nil
		if protect {
			r.network.Routes[i].Protected = true
		}
	}
	return nil
}

func TestStartupMovesUsersAndGroupsAndClosesPublicRoutes(t *testing.T) {
	r := &accessRuntime{
		policy:  config.SpaceConfig{Admins: config.AccessConfig{Users: []string{"owner@example.com"}}, Domains: []string{"example.com"}},
		network: NetworkState{RouteFound: true, Access: &config.AccessConfig{Users: []string{"visitor@example.com"}}, Routes: []PathRouteState{{Access: &config.AccessConfig{Groups: []string{"readers"}}}}},
		console: `{"exposure":"public","host":"web.example.com","access":{"users":["visitor@example.com","console@example.com"]}}`,
	}
	for range 2 {
		if err := ReconcileAccess(context.Background(), r, ""); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(r.policy.Viewers, config.AccessConfig{Users: []string{"visitor@example.com", "console@example.com"}, Groups: []string{"readers"}}) || r.policyWrites != 1 {
		t.Fatalf("migration not idempotent: %+v writes=%d", r.policy, r.policyWrites)
	}
	if !r.network.Protected || !r.network.Routes[0].Protected || r.network.Access != nil || r.network.Routes[0].Access != nil {
		t.Fatalf("unprotected or legacy routes remain: %+v", r.network)
	}
	if !strings.Contains(r.console, `"exposure":"protected"`) || strings.Contains(r.console, `"access"`) || !strings.Contains(r.console, "web.example.com") {
		t.Fatalf("console metadata = %s", r.console)
	}
	if len(r.policy.Admins.Users) != 1 || !reflect.DeepEqual(r.policy.Domains, []string{"example.com"}) {
		t.Fatal("unrelated space settings changed")
	}
}

func TestMigrationKeepsOldAccessIfSavingMembershipFails(t *testing.T) {
	r := &accessRuntime{network: NetworkState{Access: &config.AccessConfig{Users: []string{"viewer@example.com"}}}, writeError: errors.New("storage unavailable")}
	if err := ReconcileAccess(context.Background(), r, ""); err == nil || r.routeWrites != 0 || r.network.Access == nil {
		t.Fatal("old membership was lost before space membership was saved")
	}
}

func TestMigrationDeduplicatesAgainstEverySpaceRole(t *testing.T) {
	policy := config.SpaceConfig{
		Admins:     config.AccessConfig{Users: []string{"Owner@example.com", "sub:OwnerID"}, Groups: []string{"owners"}},
		Developers: config.AccessConfig{Users: []string{"developer@example.com"}, Groups: []string{"builders"}},
		Viewers:    config.AccessConfig{Users: []string{"Reader@example.com", "sub:ViewerID"}, Groups: []string{"readers"}},
	}
	r := &accessRuntime{
		policy: policy,
		network: NetworkState{RouteFound: true, Access: &config.AccessConfig{
			Users:  []string{"OWNER@example.com", "developer@example.com", "reader@example.com", "New@example.com", "new@example.com", "sub:OwnerID", "sub:ViewerID", "sub:viewerid"},
			Groups: []string{"owners", "builders", "readers", "guests", "guests", "Guests"},
		}, Routes: []PathRouteState{{Access: &config.AccessConfig{
			Users: []string{"new@EXAMPLE.COM", "route@example.com"}, Groups: []string{"guests", "shared"},
		}}}},
		console: `{"access":{"users":["NEW@example.com","route@EXAMPLE.COM","console@example.com","sub:viewerid","sub:OwnerID"],"groups":["shared","guests","console"]}}`,
	}
	for range 2 {
		if err := ReconcileAccess(context.Background(), r, ""); err != nil {
			t.Fatal(err)
		}
	}
	want := policy
	want.Viewers = config.AccessConfig{
		Users:  []string{"Reader@example.com", "sub:ViewerID", "New@example.com", "sub:viewerid", "route@example.com", "console@example.com"},
		Groups: []string{"readers", "guests", "Guests", "shared", "console"},
	}
	if !reflect.DeepEqual(r.policy, want) || r.policyWrites != 1 {
		t.Fatalf("members duplicated or existing roles changed: %+v, writes=%d", r.policy, r.policyWrites)
	}
}

func TestMigrationSkipsMembershipWriteWhenEveryoneAlreadyHasARole(t *testing.T) {
	r := &accessRuntime{
		policy: config.SpaceConfig{
			Admins:     config.AccessConfig{Users: []string{"owner@example.com"}},
			Developers: config.AccessConfig{Groups: []string{"builders"}},
		},
		network: NetworkState{Access: &config.AccessConfig{Users: []string{"OWNER@example.com"}}},
		console: `{"access":{"groups":["builders"]}}`,
	}
	if err := ReconcileAccess(context.Background(), r, ""); err != nil {
		t.Fatal(err)
	}
	if r.policyWrites != 0 || !r.policy.Viewers.Empty() || r.network.Access != nil || strings.Contains(r.console, "access") {
		t.Fatalf("existing members should only need legacy cleanup: %+v", r)
	}
}

func TestMigrationRetriesCleanupWithoutDuplicatingMembers(t *testing.T) {
	for _, fail := range []string{"routes", "console"} {
		t.Run(fail, func(t *testing.T) {
			r := &accessRuntime{
				network: NetworkState{Access: &config.AccessConfig{Users: []string{"reader@example.com"}}},
				console: `{"access":{"users":["READER@example.com","console@example.com"]}}`,
			}
			if fail == "routes" {
				r.routeError = errors.New("route write failed")
			} else {
				r.consoleError = errors.New("console write failed")
			}
			if err := ReconcileAccess(context.Background(), r, ""); err == nil || r.policyWrites != 1 || !strings.Contains(r.console, "access") {
				t.Fatalf("failed cleanup lost saved membership or migration source: %+v, %v", r, err)
			}
			r.routeError, r.consoleError = nil, nil
			if err := ReconcileAccess(context.Background(), r, ""); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.policy.Viewers.Users, []string{"reader@example.com", "console@example.com"}) || r.policyWrites != 1 || r.network.Access != nil || strings.Contains(r.console, "access") {
				t.Fatalf("retry duplicated members or left old lists: %+v", r)
			}
		})
	}
}

func TestPublicOptInAndPolicyChecks(t *testing.T) {
	r := &accessRuntime{}
	app := &config.AppConfig{Name: "web", Public: true}
	if err := CheckAppDomain(context.Background(), r, app, "team", "example.com"); err == nil {
		t.Fatal("public deployment passed default-off policy")
	}
	r.policy.AllowPublic = true
	if err := CheckAppDomain(context.Background(), r, app, "team", "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileAccess(context.Background(), r, "team"); err != nil || r.network.Protected {
		t.Fatalf("explicit opt-in was lost: %+v %v", r.network, err)
	}
	r.policy.AllowPublic = false
	if err := ReconcileAccess(context.Background(), r, "team"); err != nil || !r.network.Protected {
		t.Fatalf("disabling public did not close existing route: %+v %v", r.network, err)
	}
}
