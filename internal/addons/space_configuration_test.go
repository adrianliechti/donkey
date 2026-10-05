package addons

import (
	"encoding/json"
	"testing"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestSpaceConfigurationValidationAndLifecycle(t *testing.T) {
	schema := ObjectSchema(map[string]any{
		"tenant": map[string]any{"type": "string", "pattern": "^[a-z]+$", KeywordImmutable: true},
		"plan":   map[string]any{"type": "string", "default": "basic", "enum": []any{"basic", "enterprise"}},
	})
	schema["required"] = []any{"tenant"}
	desc := &addonv1.Offering{Id: "api", SpaceConfiguration: MustStruct(schema), Capabilities: &addonv1.Capabilities{Update: true}}
	if _, err := ResolveDefaults(desc.SpaceConfiguration, map[string]any{"plan": "enterprise"}); err != nil {
		t.Fatal("partial config cannot be saved:", err)
	}
	if _, err := PlanSpaceConfiguration(desc, nil, false, nil); err == nil {
		t.Fatal("missing required admin configuration permitted provisioning")
	}
	for _, given := range []map[string]any{{"tenant": "Bad Tenant"}, {"unknown": "value"}} {
		if _, err := ResolveDefaults(desc.SpaceConfiguration, given); err == nil {
			t.Fatalf("invalid config accepted: %v", given)
		}
	}
	configured, err := PlanSpaceConfiguration(desc, map[string]any{"tenant": "finance"}, true, nil)
	if err != nil || configured["plan"] != "basic" {
		t.Fatalf("configuration defaults: %v %v", configured, err)
	}
	record := &Provisioning{Instance: &addonv1.Instance{Installation: "main", Space: "finance", Name: "api", Offering: "api"}, SpaceConfiguration: configured, Stopped: true}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var saved Provisioning
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	resumed, err := PlanSpaceConfiguration(desc, nil, false, &saved)
	if err != nil || resumed["tenant"] != "finance" || resumed["plan"] != "basic" {
		t.Fatalf("removed offering lost resume configuration: %v %v", resumed, err)
	}
	if _, err := PlanSpaceConfiguration(desc, map[string]any{"tenant": "other"}, true, &saved); err == nil {
		t.Fatal("immutable admin setting changed")
	}
	record.Stopped, record.Info = false, sampleProvisioning()
	desc.Capabilities.Update = false
	if _, err := PlanSpaceConfiguration(desc, map[string]any{"tenant": "finance", "plan": "enterprise"}, true, record); err == nil {
		t.Fatal("changed configuration ignored update capability")
	}
}
