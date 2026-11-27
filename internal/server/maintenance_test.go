package server

import (
	"context"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/servicepolicy"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type memoryMaintenance struct {
	state  config.SpaceMaintenance
	writes int
}

func (m *memoryMaintenance) SpaceMaintenance(context.Context, string) (config.SpaceMaintenance, error) {
	return m.state, nil
}
func (m *memoryMaintenance) SetSpaceMaintenance(_ context.Context, _ string, state config.SpaceMaintenance) error {
	m.state = state
	m.writes++
	return nil
}

func TestContinuousGatewayMaintenance(t *testing.T) {
	ctx := t.Context()
	rt := &selectionRuntime{inventory: map[string][]runtime.ServiceBindingInfo{"team": {{Name: "db", Type: "postgres"}}}}
	policy, err := servicepolicy.Compile(&config.ServicePolicy{Rego: `package donkey.services
deny contains "retire the legacy database" if { some service in input.services; service.id == "postgres" }`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{rt: rt, cfg: &Config{ServicePolicy: policy}}
	store := &memoryMaintenance{}
	first := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	now := first
	rules := config.PlatformConfig{GatewayMaintenance: config.GatewayMaintenance{CriticalDays: 30, UnmaintainedDays: 7, NoncompliantDays: 7}}
	app := &runtime.AppStatus{Found: true, Image: "registry/team/api:20240101000000", Security: `{"state":"warning","summary":{"critical":1}}`}
	apps := map[string]*runtime.AppStatus{"api": app}
	observe := func() error { return s.observeSpaceMaintenance(ctx, store, "team", apps, rules, now) }
	check := func(times config.MaintenanceTimes, spaceSince int64, writes int) {
		t.Helper()
		if store.state.NoncompliantSince != spaceSince || store.state.Apps["api"] != times || store.writes != writes {
			t.Fatalf("state = %+v (%d writes), want %+v, space %d (%d writes)", store.state, store.writes, times, spaceSince, writes)
		}
	}
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	start := first.Unix()
	both := config.MaintenanceTimes{CriticalSince: start, UnmaintainedSince: start}
	check(both, start, 1)

	now = first.Add(30 * 24 * time.Hour)
	s = &Server{rt: rt, cfg: &Config{ServicePolicy: policy}} // same durable state after restart
	app.Image = "registry/team/api:20250101000000"
	app.Security = `{"state":"needs-action","summary":{"critical":2}}`
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	check(both, start, 1)
	app.Security = "" // A missing scan and an unreadable service inventory prove no resolution.
	rt.inventory["team"] = []runtime.ServiceBindingInfo{{Name: "unknown"}}
	if err := observe(); err == nil {
		t.Fatal("expected policy evaluation error")
	}
	check(both, start, 1)
	delete(rt.inventory, "team")
	apps["api"] = nil // Transient app read failure is independent of the now-compliant space.
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	check(both, 0, 2)

	apps["api"] = app
	app.Security = `{"state":"clean","summary":{"critical":0}}`
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	check(config.MaintenanceTimes{UnmaintainedSince: start}, 0, 3)
	app.Image = "registry/team/api:" + now.UTC().Format("20060102150405")
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	check(config.MaintenanceTimes{}, 0, 4)
	app.Security = `{"state":"warning","summary":{"critical":1}}`
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	check(config.MaintenanceTimes{CriticalSince: now.Unix()}, 0, 5)
	rules.GatewayMaintenance = config.GatewayMaintenance{}
	rt.inventory["team"] = []runtime.ServiceBindingInfo{{Name: "unknown"}}
	if err := observe(); err != nil {
		t.Fatalf("disabled rules evaluated inventory: %v", err)
	}
	check(config.MaintenanceTimes{}, 0, 6)
}
