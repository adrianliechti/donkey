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

func TestLegacyS3OfferingSettings(t *testing.T) {
	for _, common := range []CommonOfferings{{"s3"}, {"rustfs"}} {
		for _, id := range []string{"s3", "rustfs"} {
			if !(SpaceConfig{}).OfferingAllowed(common, id) {
				t.Fatalf("common %v no longer allows %s", common, id)
			}
		}
	}
	if err := (CommonOfferings{"s3", "rustfs"}).Validate(); err == nil {
		t.Fatal("accepted duplicate offering aliases")
	}
	legacy := SpaceOffering{Defaults: map[string]any{"storage": "5Gi"}}
	settings := SpaceConfig{Offerings: map[string]SpaceOffering{"rustfs": legacy}}
	if !settings.OfferingAllowed(CommonOfferings{"postgres"}, "s3") {
		t.Fatal("legacy space grant no longer enables S3")
	}
	entry, ok := settings.Offering("s3")
	if !ok || entry.Defaults["storage"] != "5Gi" {
		t.Fatal("legacy defaults were lost")
	}
	normalized := settings.NormalizeOfferings()
	if _, old := normalized.Offerings["rustfs"]; old || normalized.Offerings["s3"].Defaults["storage"] != "5Gi" {
		t.Fatal("canonical settings lost the legacy grant or defaults")
	}
	if _, old := settings.Offerings["rustfs"]; !old {
		t.Fatal("normalization modified the stored settings in place")
	}
	settings.Offerings["s3"] = SpaceOffering{Defaults: map[string]any{"storage": "10Gi"}}
	if normalized := settings.NormalizeOfferings(); normalized.Offerings["s3"].Defaults["storage"] != "10Gi" {
		t.Fatal("legacy defaults replaced explicit canonical settings")
	}
}
