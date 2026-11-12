package ai

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Strict output requires every object property to be required and extra
// properties forbidden. Check the schema against the structs we decode.
func TestProjectSchemaMatchesStructs(t *testing.T) {
	var check func(map[string]any, reflect.Type)
	check = func(schema map[string]any, typ reflect.Type) {
		t.Helper()
		switch typ.Kind() {
		case reflect.String:
			if schema["type"] != "string" {
				t.Fatalf("%v must be a string", typ)
			}
		case reflect.Slice:
			if schema["type"] != "array" {
				t.Fatalf("%v must be an array", typ)
			}
			check(schema["items"].(map[string]any), typ.Elem())
		case reflect.Struct:
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatalf("%v must be a strict object", typ)
			}
			props := schema["properties"].(map[string]any)
			required := schema["required"].([]string)
			if len(props) != typ.NumField() || len(required) != len(props) {
				t.Fatalf("%v has missing or extra schema fields", typ)
			}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				property, ok := props[name].(map[string]any)
				if !ok || !slices.Contains(required, name) {
					t.Fatalf("%v.%s must be present and required", typ, name)
				}
				check(property, field.Type)
			}
		default:
			t.Fatalf("unhandled field type %v", typ)
		}
	}
	check(projectSchema, reflect.TypeFor[Project]())
	kind := projectConnectionSchema["properties"].(map[string]any)["kind"].(map[string]any)
	if !slices.Equal(kind["enum"].([]string), []string{"browser", "server", "address", "origin", "service"}) {
		t.Fatal("connection kinds must be restricted to supported values")
	}
}
