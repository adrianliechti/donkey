package addons

import (
	"context"
	"reflect"
	"strings"
	"testing"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestBuiltinDescriptorsAreValid(t *testing.T) {
	for _, a := range Types() {
		desc, err := a.Describe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if desc.Type != a.Type() || desc.Title == "" || desc.Protocol == "" || len(desc.Env) == 0 {
			t.Fatalf("%s: incomplete descriptor %+v", a.Type(), desc)
		}
		if err := validateDescription(a.Type(), desc); err != nil {
			t.Fatalf("%s: %v", a.Type(), err)
		}
		params, err := ResolveParameters(desc.Parameters, nil)
		if err != nil {
			t.Fatalf("%s: defaults do not resolve: %v", a.Type(), err)
		}
		if a.Type() != "openai" && (params.String(ParamVersion) == "" || params.String(ParamStorage) == "") {
			t.Fatalf("%s: version and storage defaults missing: %v", a.Type(), params)
		}
		if a.Type() == "openai" && len(params) != 0 {
			t.Fatalf("openai takes no parameters: %v", params)
		}
	}
}

func TestParameterResolutionDefaultsCoercionAndErrors(t *testing.T) {
	schema := MustStruct(ObjectSchema(map[string]any{
		ParamVersion: VersionParameter("18.6", "tag"),
		ParamStorage: StorageParameter("1Gi"),
		"replicas":   map[string]any{"type": "integer", "minimum": 1, "maximum": 3, "default": 1},
		"public":     map[string]any{"type": "boolean", "default": false},
		"hosts":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}))
	params, err := ResolveParameters(schema, map[string]any{ParamVersion: 16, "replicas": "2", "public": "true", "hosts": "a, b"})
	if err != nil {
		t.Fatal(err)
	}
	want := Parameters{ParamVersion: "16", ParamStorage: "1Gi", "replicas": float64(2), "public": true, "hosts": []any{"a", "b"}}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("resolved %v, want %v", params, want)
	}
	for name, given := range map[string]map[string]any{
		"unknown":       {"disk": "5Gi"},
		"bad quantity":  {ParamStorage: "five gigs"},
		"out of range":  {"replicas": 9},
		"wrong type":    {"replicas": "many"},
		"bad version":   {ParamVersion: "a b"},
		"array of ints": {"hosts": 3},
	} {
		if _, err := ResolveParameters(schema, given); err == nil {
			t.Fatalf("%s: accepted %v", name, given)
		}
	}
	if _, err := ResolveParameters(schema, map[string]any{"disk": "5Gi"}); err == nil || !strings.Contains(err.Error(), "hosts, public, replicas, storage, version") {
		t.Fatalf("unknown parameter error does not list the known ones: %v", err)
	}
	if params, err := ResolveParameters(nil, nil); err != nil || len(params) != 0 {
		t.Fatalf("no schema means no parameters: %v %v", params, err)
	}
	if _, err := ResolveParameters(nil, map[string]any{"x": 1}); err == nil {
		t.Fatal("parameters accepted by a type without any")
	}
}

func TestInstanceParametersKeepStoredValuesAndRefuseImmutableChanges(t *testing.T) {
	desc := &addonv1.DescribeResponse{Parameters: MustStruct(ObjectSchema(map[string]any{
		ParamVersion: VersionParameter("18.6", "tag"),
		ParamStorage: StorageParameter("1Gi"),
	}))}
	stored := Parameters{ParamVersion: "17", ParamStorage: "5Gi", "retired": "x"}
	params, err := ResolveInstanceParameters(desc, nil, stored)
	if err != nil || params.String(ParamVersion) != "17" || params.String(ParamStorage) != "5Gi" || len(params) != 2 {
		t.Fatalf("stored values not kept (or retired one not dropped): %v %v", params, err)
	}
	params, err = ResolveInstanceParameters(desc, map[string]any{ParamVersion: "18"}, stored)
	if err != nil || params.String(ParamVersion) != "18" || params.String(ParamStorage) != "5Gi" {
		t.Fatalf("partial request lost the stored disk: %v %v", params, err)
	}
	if _, err := ResolveInstanceParameters(desc, map[string]any{ParamStorage: "10Gi"}, stored); err == nil || !strings.Contains(err.Error(), "storage cannot change") {
		t.Fatalf("immutable disk size changed: %v", err)
	}
	if _, err := ResolveInstanceParameters(desc, map[string]any{ParamStorage: "10Gi"}, nil); err != nil {
		t.Fatalf("a new instance may pick any size: %v", err)
	}
}

func TestSchemaSubsetIsEnforced(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"not an object": {"type": "string"},
		"nested object": ObjectSchema(map[string]any{"limits": map[string]any{"type": "object"}}),
		"reference":     ObjectSchema(map[string]any{"size": map[string]any{"$ref": "#/$defs/size"}}),
		"array of objects": ObjectSchema(map[string]any{"rules": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}),
		"invalid default":  ObjectSchema(map[string]any{"n": map[string]any{"type": "integer", "default": "one"}}),
	} {
		if err := ValidateSchema(MustStruct(schema)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := ValidateSchema(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilitiesAndRuntimes(t *testing.T) {
	desc := &addonv1.DescribeResponse{Capabilities: []string{"runtime:kubernetes", CapabilityKeepData}}
	if !SupportsRuntime(desc, "kubernetes") || SupportsRuntime(desc, "docker") || !HasCapability(desc, CapabilityKeepData) {
		t.Fatal("capabilities misread")
	}
	if !SupportsRuntime(&addonv1.DescribeResponse{}, "docker") || SupportsRuntime(nil, "docker") {
		t.Fatal("an unlisted runtime set means every runtime; nil means none")
	}
}
