package config

import "fmt"

// GatewayMaintenance defines grace periods in days. Zero disables a rule.
type GatewayMaintenance struct {
	CriticalDays     int `json:"criticalDays,omitempty" yaml:"criticalDays,omitempty"`
	UnmaintainedDays int `json:"unmaintainedDays,omitempty" yaml:"unmaintainedDays,omitempty"`
	NoncompliantDays int `json:"noncompliantDays,omitempty" yaml:"noncompliantDays,omitempty"`
}

func (g GatewayMaintenance) validate() error {
	for _, days := range []int{g.CriticalDays, g.UnmaintainedDays, g.NoncompliantDays} {
		if days < 0 || days > 3650 {
			return fmt.Errorf("gateway maintenance grace periods must be between 0 and 3650 days")
		}
	}
	return nil
}

// MaintenanceTimes contains only Unix timestamps. No findings or service
// bindings need to travel to the public gateway.
type MaintenanceTimes struct {
	CriticalSince     int64 `json:"criticalSince,omitempty" yaml:"criticalSince,omitempty"`
	UnmaintainedSince int64 `json:"unmaintainedSince,omitempty" yaml:"unmaintainedSince,omitempty"`
}

type SpaceMaintenance struct {
	NoncompliantSince int64                       `json:"noncompliantSince,omitempty" yaml:"noncompliantSince,omitempty"`
	Apps              map[string]MaintenanceTimes `json:"apps,omitempty" yaml:"apps,omitempty"`
}

// Deadline is calculated when the route table refreshes. The request path
// only compares this timestamp to the clock.
func (s SpaceMaintenance) Deadline(app string, rules GatewayMaintenance) int64 {
	var earliest int64
	add := func(since int64, days int) {
		if since <= 0 || days <= 0 {
			return
		}
		at := since + int64(days)*86400
		if earliest == 0 || at < earliest {
			earliest = at
		}
	}
	times := s.Apps[app]
	add(times.CriticalSince, rules.CriticalDays)
	add(times.UnmaintainedSince, rules.UnmaintainedDays)
	add(s.NoncompliantSince, rules.NoncompliantDays)
	return earliest
}
