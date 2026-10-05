package config

import "testing"

func TestOfferingAvailability(t *testing.T) {
	space := SpaceConfig{Offerings: map[string]SpaceOffering{"mariadb": {}}}
	for _, test := range []struct {
		common  CommonOfferings
		id      string
		allowed bool
	}{
		{nil, "anything", true}, {CommonOfferings{}, "anything", true},
		{CommonOfferings{"postgres"}, "postgres", true},
		{CommonOfferings{"postgres"}, "mariadb", true},
		{CommonOfferings{"postgres"}, "redis", false},
	} {
		if got := space.OfferingAllowed(test.common, test.id); got != test.allowed {
			t.Errorf("common=%v, offering=%s: allowed=%t", test.common, test.id, got)
		}
	}
	for _, ids := range []CommonOfferings{{"postgres", "postgres"}, {"Bad ID"}} {
		if err := ids.Validate(); err == nil {
			t.Errorf("invalid common offerings accepted: %v", ids)
		}
	}
	if err := (SpaceConfig{Offerings: map[string]SpaceOffering{"Bad ID": {}}}).Validate(); err == nil {
		t.Fatal("invalid space offering id accepted")
	}
}
