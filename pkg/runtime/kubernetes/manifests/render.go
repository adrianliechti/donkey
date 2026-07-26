package manifests

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Render turns objects into the YAML stream `--dry-run` prints. Nulls that
// only exist because of Go zero values (creationTimestamp, empty status) are
// pruned so the output reads like hand-written manifests.
func Render(objs []runtime.Object) (string, error) {
	var parts []string
	for _, obj := range objs {
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return "", err
		}
		prune(u)
		b, err := yaml.Marshal(u)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "---\n"), nil
}

// prune removes creationTimestamp: null (recursively) and empty status.
func prune(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if ct, ok := m["creationTimestamp"]; ok && ct == nil {
		delete(m, "creationTimestamp")
	}
	for _, key := range []string{"status", "spec"} {
		if v, ok := m[key]; ok {
			if vm, isMap := v.(map[string]any); isMap && len(vm) == 0 {
				delete(m, key)
			}
		}
	}
	for _, val := range m {
		switch t := val.(type) {
		case map[string]any:
			prune(t)
		case []any:
			for _, item := range t {
				prune(item)
			}
		}
	}
}
