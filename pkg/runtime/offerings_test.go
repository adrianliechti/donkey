package runtime

import (
	"context"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

type offeringRuntime struct {
	policy   config.SpaceConfig
	attached bool
	retained bool
}

func (r offeringRuntime) SpaceConfig(context.Context, string) (config.SpaceConfig, error) {
	return r.policy, nil
}
func (r offeringRuntime) ServiceBinding(context.Context, string, string, string) (map[string]string, error) {
	if r.attached {
		return map[string]string{"URL": "saved"}, nil
	}
	return nil, nil
}
func (r offeringRuntime) ServiceConnection(context.Context, string, string) (ServiceConnection, error) {
	if r.retained {
		return ServiceConnection{}, nil
	}
	return ServiceConnection{Type: "db", Binding: map[string]string{"URL": "saved"}}, nil
}
func (r offeringRuntime) ListServices(context.Context, string, string) ([]ServiceBindingInfo, error) {
	return []ServiceBindingInfo{{Name: "db", Type: "db", State: addons.StateStopped}}, nil
}

func TestSpaceDefaultsAndNewConsumerPolicy(t *testing.T) {
	ctx := context.Background()
	desc := &addonv1.Offering{Id: "db", Parameters: addons.MustStruct(addons.ObjectSchema(map[string]any{
		"storage": addons.StorageParameter("1Gi"), "version": addons.VersionParameter("1", "tag"),
	}))}
	r := offeringRuntime{policy: config.SpaceConfig{Offerings: map[string]config.SpaceOffering{
		"db": {Defaults: map[string]any{"storage": "10Gi", "version": "2"}},
	}}}
	params, err := PrepareService(ctx, r, config.CommonOfferings{"redis"}, "team", desc, map[string]any{"version": "3"}, false)
	if err != nil || params["storage"] != "10Gi" || params["version"] != "3" {
		t.Fatalf("default precedence: %v %v", params, err)
	}
	params, err = PrepareService(ctx, r, nil, "team", desc, nil, true)
	if err != nil || len(params) != 0 {
		t.Fatal("space defaults changed an existing instance")
	}
	r.policy.Offerings = nil
	if _, err := PrepareService(ctx, r, config.CommonOfferings{"redis"}, "team", desc, nil, false); err == nil {
		t.Fatal("disabled offering created a service")
	}
	if _, err := PrepareService(ctx, r, config.CommonOfferings{}, "team", desc, nil, false); err != nil {
		t.Fatal("empty commonOfferings did not allow all:", err)
	}
	for _, retained := range []bool{false, true} {
		r.retained = retained
		if err := CheckServiceAttachment(ctx, r, config.CommonOfferings{"redis"}, "team", "api", "db"); err == nil {
			t.Fatal("disabled existing service admitted a new consumer")
		}
		r.attached = true
		if err := CheckServiceAttachment(ctx, r, config.CommonOfferings{"redis"}, "team", "api", "db"); err != nil {
			t.Fatal("policy change disrupted an existing consumer:", err)
		}
		r.attached = false
	}
}

func TestLegacyS3DefaultsApplyOnlyAtCreation(t *testing.T) {
	ctx := context.Background()
	addon, err := addons.Get(ctx, "s3")
	if err != nil {
		t.Fatal(err)
	}
	desc, err := addon.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := offeringRuntime{policy: config.SpaceConfig{Offerings: map[string]config.SpaceOffering{
		"rustfs": {Defaults: map[string]any{"storage": "5Gi"}},
	}}}
	params, err := PrepareService(ctx, r, config.CommonOfferings{"postgres"}, "team", desc, nil, false)
	if err != nil || params["storage"] != "5Gi" {
		t.Fatalf("legacy grant or defaults lost: %v %v", params, err)
	}
	params, err = PrepareService(ctx, r, config.CommonOfferings{"postgres"}, "team", desc, nil, true)
	if err != nil || len(params) != 0 {
		t.Fatalf("legacy defaults changed an existing disk: %v %v", params, err)
	}
}
