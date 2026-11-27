package config

import "testing"

func TestGatewayMaintenanceDeadlines(t *testing.T) {
	const day int64 = 24 * 60 * 60
	state := SpaceMaintenance{NoncompliantSince: 5 * day, Apps: map[string]MaintenanceTimes{
		"api": {CriticalSince: day, UnmaintainedSince: 4 * day},
	}}
	for _, test := range []struct {
		app   string
		rules GatewayMaintenance
		want  int64
	}{
		{"api", GatewayMaintenance{}, 0},
		{"api", GatewayMaintenance{CriticalDays: 30}, 31 * day},
		{"api", GatewayMaintenance{UnmaintainedDays: 7}, 11 * day},
		{"api", GatewayMaintenance{NoncompliantDays: 2}, 7 * day},
		{"api", GatewayMaintenance{CriticalDays: 30, UnmaintainedDays: 1, NoncompliantDays: 2}, 5 * day},
		{"other", GatewayMaintenance{CriticalDays: 1}, 0},
		{"other", GatewayMaintenance{NoncompliantDays: 2}, 7 * day},
	} {
		if got := state.Deadline(test.app, test.rules); got != test.want {
			t.Errorf("Deadline(%q, %+v) = %d, want %d", test.app, test.rules, got, test.want)
		}
	}
	for _, rules := range []GatewayMaintenance{{CriticalDays: -1}, {UnmaintainedDays: 3651}, {NoncompliantDays: -1}} {
		if (PlatformConfig{GatewayMaintenance: rules}).Validate() == nil {
			t.Errorf("accepted invalid grace period %+v", rules)
		}
	}
}
