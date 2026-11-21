package packages

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

var npmRegistryTarball = regexp.MustCompile("^" + npmTarballURL + "$")

// npm can install a locked name and version without a resolved field. Let it
// look up the tarball in the build's registry, which may serve the same bytes
// under a different filename or with a fresh signed URL.
func rewriteNPMLockfile(content, _ string, declared []string) string {
	var lock map[string]any
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	if decoder.Decode(&lock) != nil || decoder.Decode(new(any)) != io.EOF {
		return content
	}

	var entries []map[string]any
	packages, _ := lock["packages"].(map[string]any)
	for _, value := range packages {
		if entry, ok := value.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	// npm v1 records a nested dependency tree. v2 has both representations.
	var visitDependencies func(map[string]any)
	visitDependencies = func(parent map[string]any) {
		children, _ := parent["dependencies"].(map[string]any)
		for _, child := range children {
			if entry, ok := child.(map[string]any); ok {
				entries = append(entries, entry)
				visitDependencies(entry)
			}
		}
	}
	visitDependencies(lock)

	// A URL requested by a dependency is part of its identity, even when
	// its path looks exactly like a registry download.
	requested := make(map[string]bool)
	for _, entry := range entries {
		for _, key := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies", "requires"} {
			specs, _ := entry[key].(map[string]any)
			for _, spec := range specs {
				if address, ok := spec.(string); ok && (strings.HasPrefix(address, "https://") || strings.HasPrefix(address, "http://")) {
					requested[address] = true
				}
			}
		}
	}

	changed := false
	for _, entry := range entries {
		address, _ := entry["resolved"].(string)
		version, _ := entry["version"].(string)
		if version == "" || version == address || requested[address] || under(address, declared) {
			continue
		}
		path, _, _ := strings.Cut(address, "?")
		path, _, _ = strings.Cut(path, "#")
		if npmRegistryTarball.MatchString(path) {
			delete(entry, "resolved")
			changed = true
		}
	}
	if !changed {
		return content
	}
	data, err := json.Marshal(lock)
	if err != nil {
		return content
	}
	return string(data)
}
