package config

import (
	"encoding/json"
	"fmt"
)

// CommonOfferings is the catalog available in every space. An empty list
// allows all offerings; space admins can add to a nonempty list.
type CommonOfferings []string

func (ids CommonOfferings) Validate() error {
	if len(ids) > 256 {
		return fmt.Errorf("commonOfferings supports at most 256 entries")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		canonical := NormalizeAddonType(id)
		if !ValidName(id) || seen[canonical] {
			return fmt.Errorf("commonOfferings: %q must be a unique lowercase name", id)
		}
		seen[canonical] = true
	}
	return nil
}

func (ids CommonOfferings) JSON() string {
	data, _ := json.Marshal(ids)
	return string(data)
}
