package config

import (
	"encoding/json"
	"fmt"
)

// RemoveLegacyRoles is only for persisted membership records, never new
// configuration. The named fields contain memberships; "" means the record
// itself is a membership. No role name is reinterpreted as a user or group.
// A nil result means nothing changed. Other fields remain intact so the normal
// strict decoder can still reject malformed configuration after migration.
func RemoveLegacyRoles(data []byte, fields ...string) ([]byte, []string, error) {
	var record map[string]any
	if err := DecodeYAML(data, &record); err != nil {
		return nil, nil, err
	}
	changed := false
	var removed []string
	for _, field := range fields {
		members := record
		if field != "" {
			members, _ = record[field].(map[string]any)
		}
		roles, found := members["roles"]
		if !found {
			continue
		}
		// Validate the obsolete field too; malformed grants are not discarded.
		raw, err := json.Marshal(roles)
		if err != nil {
			return nil, nil, err
		}
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, nil, fmt.Errorf("legacy roles must be a list of strings: %w", err)
		}
		for _, name := range names {
			if field != "" {
				name = field + ".roles:" + name
			}
			removed = append(removed, name)
		}
		delete(members, "roles")
		changed = true
	}
	if !changed {
		return nil, nil, nil
	}
	// JSON is also valid YAML and preserves JSON annotations and metadata.
	raw, err := json.Marshal(record)
	return raw, removed, err
}
