package addons

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/protobuf/types/known/structpb"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// The parameters every built-in reads, and the extension keywords the console
// understands on top of the JSON Schema subset described in the plugin README.
const (
	ParamVersion = "version"
	ParamStorage = "storage"

	// KeywordWidget picks a console control: "disk-size" offers a ladder of
	// sizes, "select" forces a dropdown.
	KeywordWidget = "x-donkey-widget"
	// KeywordImmutable marks a value that cannot change once the service
	// exists (a disk cannot shrink; a database engine cannot swap).
	KeywordImmutable = "x-donkey-immutable"
	// KeywordAdvanced hides the field behind an "advanced" toggle.
	KeywordAdvanced = "x-donkey-advanced"
	// KeywordOrder sorts fields (ascending); unordered ones follow by name.
	KeywordOrder = "x-donkey-order"

	// FormatQuantity is a Kubernetes-style size such as 1Gi or 500Mi.
	FormatQuantity = "quantity"
)

var quantityPattern = `^[0-9]+(\.[0-9]+)?([KMGTPE]i?)?$`

// Parameters are the resolved values of a descriptor's schema, as decoded
// JSON (strings, float64, bool, and so on).
type Parameters map[string]any

// String renders one value the way a person or an image tag wants it.
func (p Parameters) String(key string) string {
	switch v := p[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// Struct converts to the wire form; empty parameters travel as nil.
func (p Parameters) Struct() *structpb.Struct {
	if len(p) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(p)
	if err != nil {
		return nil
	}
	return s
}

// ParametersFrom decodes the wire form.
func ParametersFrom(s *structpb.Struct) Parameters {
	if s == nil {
		return nil
	}
	return Parameters(s.AsMap())
}

// MustStruct converts a literal schema; only programmer errors panic.
func MustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

// ObjectSchema wraps property schemas into the object schema a descriptor
// carries. Only declared parameters exist, whatever the schema says about
// additional properties.
func ObjectSchema(properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "additionalProperties": false}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	return schema
}

// VersionParameter is the image-tag style version every built-in offers.
func VersionParameter(defaultVersion, description string) map[string]any {
	return map[string]any{
		"type": "string", "title": "Version", "description": description,
		"default": defaultVersion, "pattern": `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`,
		KeywordAdvanced: true, KeywordOrder: 90,
	}
}

// StorageParameter is the data disk size; it cannot change after creation.
func StorageParameter(defaultSize string) map[string]any {
	return map[string]any{
		"type": "string", "title": "Disk", "description": "Size of the data disk, such as 1Gi or 10Gi.",
		"default": defaultSize, "format": FormatQuantity, "pattern": quantityPattern,
		KeywordWidget: "disk-size", KeywordImmutable: true, KeywordOrder: 10,
	}
}

var scalarTypes = []string{"string", "integer", "number", "boolean"}

// compiled is a parameter schema ready to validate against.
type compiled struct {
	resolved *jsonschema.Resolved
	schema   *jsonschema.Schema
}

// knows reports whether the schema declares a parameter. Only declared
// parameters exist: a typo fails fast, and a stored value the schema no
// longer names is dropped rather than carried along.
func (c *compiled) knows(name string) bool {
	_, ok := c.schema.Properties[name]
	return ok
}

// compileSchema accepts the renderable subset: one object whose properties
// are scalars or arrays of scalars, without references. An absent schema
// means "no parameters".
func compileSchema(s *structpb.Struct) (*compiled, error) {
	if s == nil || len(s.Fields) == 0 {
		s = MustStruct(ObjectSchema(nil))
	}
	raw, err := json.Marshal(s.AsMap())
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if schema.Type != "object" || schema.Ref != "" || len(schema.Defs) > 0 || len(schema.Definitions) > 0 {
		return nil, fmt.Errorf("the parameter schema must be one object without references")
	}
	for name, prop := range schema.Properties {
		if prop == nil || prop.Ref != "" {
			return nil, fmt.Errorf("parameter %s must be a scalar schema without references", name)
		}
		typ := prop.Type
		if typ == "array" {
			if prop.Items == nil || !slices.Contains(scalarTypes, prop.Items.Type) {
				return nil, fmt.Errorf("parameter %s must be an array of scalars", name)
			}
			continue
		}
		if !slices.Contains(scalarTypes, typ) {
			return nil, fmt.Errorf("parameter %s must be one of %s or an array of them", name, strings.Join(scalarTypes, ", "))
		}
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return nil, err
	}
	return &compiled{resolved: resolved, schema: &schema}, nil
}

// ValidateSchema rejects descriptors the console could not render.
func ValidateSchema(s *structpb.Struct) error {
	_, err := compileSchema(s)
	return err
}

// ResolveParameters applies the schema's top-level defaults to the given
// values and validates the result. Names the schema does not declare are an
// error. Values are normalized to their JSON forms, so callers may pass Go
// ints or the console's decoded numbers alike.
func ResolveParameters(s *structpb.Struct, given map[string]any) (Parameters, error) {
	c, err := compileSchema(s)
	if err != nil {
		return nil, err
	}
	schema := c.schema
	params := map[string]any{}
	if len(given) > 0 {
		raw, err := json.Marshal(given)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	for name := range params {
		if !c.knows(name) {
			return nil, fmt.Errorf("unknown parameter %q (this service takes %s)", name, describeNames(schema.Properties))
		}
	}
	for name, prop := range schema.Properties {
		if _, ok := params[name]; ok || len(prop.Default) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(prop.Default, &value); err != nil {
			return nil, err
		}
		params[name] = value
	}
	for name, prop := range schema.Properties {
		if value, ok := params[name]; ok {
			params[name] = coerce(prop, value)
		}
	}
	if err := c.resolved.Validate(params); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}
	return Parameters(params), nil
}

// coerce meets flags and YAML halfway: "16" for a string is fine, 16 for a
// string becomes "16", "3" for an integer becomes 3, and "a,b" fills an
// array. Anything else is left for validation to judge.
func coerce(prop *jsonschema.Schema, value any) any {
	if prop == nil {
		return value
	}
	switch prop.Type {
	case "string":
		switch v := value.(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		}
	case "integer", "number":
		if s, ok := value.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f
			}
		}
	case "boolean":
		if s, ok := value.(string); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				return b
			}
		}
	case "array":
		if s, ok := value.(string); ok {
			var items []any
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					items = append(items, coerce(prop.Items, part))
				}
			}
			return items
		}
	}
	return value
}

func describeNames(properties map[string]*jsonschema.Schema) string {
	if len(properties) == 0 {
		return "no parameters"
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// CheckImmutable rejects changes to values marked immutable, comparing the
// stored parameters of an existing instance with the requested ones.
func CheckImmutable(s *structpb.Struct, previous, next Parameters) error {
	if len(previous) == 0 {
		return nil
	}
	c, err := compileSchema(s)
	if err != nil {
		return err
	}
	for name, prop := range c.schema.Properties {
		if immutable, _ := prop.Extra[KeywordImmutable].(bool); !immutable {
			continue
		}
		if _, was := previous[name]; !was {
			continue
		}
		if !jsonschema.Equal(previous[name], next[name]) {
			return fmt.Errorf("%s cannot change once the service exists (it is %s); remove the service to pick another", name, previous.String(name))
		}
	}
	return nil
}

// ResolveInstanceParameters resolves a request against an instance that may
// already exist. Explicit values overlay the stored ones (so a partial
// request keeps the rest), values the schema no longer knows are dropped,
// defaults fill the gaps, and immutable values may not change.
func ResolveInstanceParameters(desc *addonv1.Offering, given map[string]any, previous Parameters) (Parameters, error) {
	c, err := compileSchema(desc.GetParameters())
	if err != nil {
		return nil, err
	}
	merged := map[string]any{}
	for name, value := range previous {
		if c.knows(name) {
			merged[name] = value
		}
	}
	for name, value := range given {
		merged[name] = value
	}
	params, err := ResolveParameters(desc.GetParameters(), merged)
	if err != nil {
		return nil, err
	}
	if err := CheckImmutable(desc.GetParameters(), previous, params); err != nil {
		return nil, err
	}
	return params, nil
}
