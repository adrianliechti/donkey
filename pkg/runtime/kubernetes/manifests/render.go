package manifests

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Render turns objects into the YAML stream `--dry-run` prints. Nulls that
// only exist because of Go zero values (creationTimestamp, empty status) are
// pruned so the output reads like hand-written manifests.
func Render(objs []runtime.Object) (string, error) {
	return render(objs, "")
}

// RenderForInstallation renders objects meant to be applied outside donkey's
// Kubernetes client. Apply normally stamps this ownership label at the API
// boundary; a generated manifest needs the same identity in the YAML itself.
func RenderForInstallation(objs []runtime.Object, installation string) (string, error) {
	return render(objs, installation)
}

func render(objs []runtime.Object, installation string) (string, error) {
	var parts []string
	for _, obj := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return "", err
		}
		u := &unstructured.Unstructured{Object: m}
		if installation != "" {
			if err := StampInstallation(u, installation); err != nil {
				return "", err
			}
		}
		prune(u.Object)
		b, err := yaml.Marshal(u.Object)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "---\n"), nil
}

// StampInstallation applies the ownership boundary to an object and to every
// selector/template that creates or targets its pods. It also guarantees the
// standard managed-by marker, and — where the labels name no instance of
// their own — the standard instance label: control-plane objects belong to
// the installation, which is exactly what distinguishes two donkeys on one
// cluster. Selectors stay minimal identity and never gain the descriptive
// instance label. Apply and offline rendering share this function so their
// manifests cannot drift apart.
func StampInstallation(u *unstructured.Unstructured, installation string) error {
	if installation == "" {
		return fmt.Errorf("installation label cannot be empty")
	}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManagedBy] = ManagedByValue
	labels[LabelInstallation] = installation
	if _, ok := labels[LabelInstance]; !ok {
		labels[LabelInstance] = installation
	}
	u.SetLabels(labels)

	var selectorPaths, labelPaths [][]string
	switch u.GetKind() {
	case "Deployment":
		selectorPaths = append(selectorPaths, []string{"spec", "selector", "matchLabels"})
		labelPaths = append(labelPaths, []string{"spec", "template", "metadata", "labels"})
	case "Job":
		labelPaths = append(labelPaths, []string{"spec", "template", "metadata", "labels"})
	case "Service":
		// A selector-less Service is intentional (for example ExternalName).
		if _, found, err := unstructured.NestedStringMap(u.Object, "spec", "selector"); err != nil {
			return fmt.Errorf("reading Service %s selector: %w", u.GetName(), err)
		} else if found {
			selectorPaths = append(selectorPaths, []string{"spec", "selector"})
		}
	case "NetworkPolicy":
		selectorPaths = append(selectorPaths, []string{"spec", "podSelector", "matchLabels"})
	case "Certificate":
		// cert-manager copies these labels to the generated TLS Secret.
		labelPaths = append(labelPaths, []string{"spec", "secretTemplate", "labels"})
	}
	stamp := func(path []string, fillInstance bool) error {
		values, _, err := unstructured.NestedStringMap(u.Object, path...)
		if err != nil {
			return fmt.Errorf("reading %s %s %s: %w", u.GetKind(), u.GetName(), strings.Join(path, "."), err)
		}
		if values == nil {
			values = map[string]string{}
		}
		values[LabelInstallation] = installation
		if fillInstance {
			if _, ok := values[LabelInstance]; !ok {
				values[LabelInstance] = installation
			}
		}
		if err := unstructured.SetNestedStringMap(u.Object, values, path...); err != nil {
			return fmt.Errorf("stamping %s %s %s: %w", u.GetKind(), u.GetName(), strings.Join(path, "."), err)
		}
		return nil
	}
	for _, path := range selectorPaths {
		if err := stamp(path, false); err != nil {
			return err
		}
	}
	for _, path := range labelPaths {
		if err := stamp(path, true); err != nil {
			return err
		}
	}
	return nil
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
