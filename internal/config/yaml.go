package config

import "go.yaml.in/yaml/v4"

// EncodeYAML writes human-editable configuration with consistent indentation.
func EncodeYAML(value any) ([]byte, error) {
	return yaml.Dump(value, yaml.WithIndent(2))
}

// DecodeYAML rejects unknown fields, duplicate keys and multiple documents.
// JSON is a YAML subset, so existing JSON settings can be migrated directly.
func DecodeYAML(data []byte, value any) error {
	return yaml.Load(data, value, yaml.WithKnownFields())
}
