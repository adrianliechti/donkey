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
	return render(objs, "")
}

// RenderForInstance renders objects meant to be applied outside donkey's
// Kubernetes client. Apply normally stamps this ownership label at the API
// boundary; a generated manifest needs the same identity in the YAML itself.
func RenderForInstance(objs []runtime.Object, instance string) (string, error) {
	return render(objs, instance)
}

func render(objs []runtime.Object, instance string) (string, error) {
	var parts []string
	for _, obj := range objs {
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return "", err
		}
		if instance != "" {
			metadata, _ := u["metadata"].(map[string]any)
			if metadata == nil {
				metadata = map[string]any{}
				u["metadata"] = metadata
			}
			labels, _ := metadata["labels"].(map[string]any)
			if labels == nil {
				labels = map[string]any{}
				metadata["labels"] = labels
			}
			labels[LabelInstance] = instance
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
