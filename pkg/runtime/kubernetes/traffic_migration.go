package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func legacyAppCertificateName(space, app string) string { return "donkey-app-" + space + "." + app }

func legacyAppTrafficName(kind, space, app string) string {
	switch kind {
	case "Certificate":
		return legacyAppCertificateName(space, app)
	case "DNSEndpoint":
		return legacyAppCertificateName(space, app) + "-dns"
	default:
		return ""
	}
}

// legacyAppTrafficObject checks the previous name before applying a replacement.
// Only the app's exact owned object in the system namespace may be migrated.
func (c *Client) legacyAppTrafficObject(ctx context.Context, next *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	kind := next.GetKind()
	identity := next.GetLabels()
	space, app := identity[manifests.LabelSpace], identity[manifests.LabelApp]
	if next.GetNamespace() != c.installation() || identity[manifests.LabelRole] != manifests.RoleEdge ||
		!config.ValidSpaceName(space) || !config.ValidName(app) {
		return nil, nil
	}
	name := legacyAppTrafficName(kind, space, app)
	if name == "" || name == next.GetName() {
		return nil, nil
	}
	currentName := manifests.AppCertificateName(space, app)
	if kind == "DNSEndpoint" {
		currentName = manifests.AppDNSName(space, app)
	}
	if next.GetName() != currentName {
		return nil, nil
	}
	old, err := c.Dynamic.Resource(gvrFor[kind]).Namespace(next.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject(kind, old.GetNamespace(), old.GetName(), old.GetLabels(), identity); err != nil {
		return nil, err
	}
	return old, nil
}

// preserveAppTLSSecret also handles installations upgrading directly from the
// original TLS Secret name. An already migrated Secret takes precedence.
func (c *Client) preserveAppTLSSecret(ctx context.Context, old, next *unstructured.Unstructured) error {
	oldName, _, _ := unstructured.NestedString(old.Object, "spec", "secretName")
	nextName, _, _ := unstructured.NestedString(next.Object, "spec", "secretName")
	if oldName == "" || nextName == "" || oldName == nextName {
		return nil
	}
	secret, err := c.Clientset.CoreV1().Secrets(old.GetNamespace()).Get(ctx, oldName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Issuance may still be pending; the replacement Certificate resumes it.
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Secret", secret.Namespace, secret.Name, secret.Labels, next.GetLabels()); err != nil {
		return err
	}
	if secret.Type != corev1.SecretTypeTLS {
		return fmt.Errorf("Secret %s/%s is not a TLS Secret", secret.Namespace, secret.Name)
	}
	return c.copySecret(ctx, secret, nextName)
}

// The replacement is applied first, so a failed apply leaves the old resource
// intact. Orphan deletion preserves TLS Secrets even when cert-manager's
// certificate owner references are enabled.
func (c *Client) deleteLegacyAppTrafficObject(ctx context.Context, old *unstructured.Unstructured) error {
	opts := metav1.DeleteOptions{}
	if uid := old.GetUID(); uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if old.GetKind() == "Certificate" {
		opts.PropagationPolicy = new(metav1.DeletePropagationOrphan)
	}
	err := c.Dynamic.Resource(gvrFor[old.GetKind()]).Namespace(old.GetNamespace()).Delete(ctx, old.GetName(), opts)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing legacy %s %s: %w", old.GetKind(), old.GetName(), err)
	}
	return nil
}

// cert-manager also records the owning Certificate's name in a Secret
// annotation. Leaving that old name makes the new Certificate report
// IncorrectCertificate even though its key and signed certificate are valid.
func (c *Client) finishAppCertificateMigration(ctx context.Context, next *unstructured.Unstructured) error {
	identity := next.GetLabels()
	space, app := identity[manifests.LabelSpace], identity[manifests.LabelApp]
	if next.GetNamespace() != c.installation() || identity[manifests.LabelRole] != manifests.RoleEdge ||
		!config.ValidSpaceName(space) || !config.ValidName(app) || next.GetName() != manifests.AppCertificateName(space, app) {
		return nil
	}
	name, _, _ := unstructured.NestedString(next.Object, "spec", "secretName")
	if name == "" {
		return nil
	}
	secrets := c.Clientset.CoreV1().Secrets(next.GetNamespace())
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	const certificateNameAnnotation = "cert-manager.io/certificate-name"
	if secret.Annotations[certificateNameAnnotation] != legacyAppCertificateName(space, app) {
		return nil
	}
	if err := c.requireOwnedObject("Secret", secret.Namespace, name, secret.Labels, identity); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": secret.UID, "annotations": map[string]string{certificateNameAnnotation: next.GetName()},
	}})
	if err != nil {
		return err
	}
	_, err = secrets.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-config-migration"})
	return err
}

// migrateAppTrafficNames covers server restarts as well as installer upgrades.
// Preserve the live spec; normal traffic reconciliation applies setting changes.
func (r *Runtime) migrateAppTrafficNames(ctx context.Context) error {
	var kinds []string
	if r.opts.TLS.UsesCertManager() {
		kinds = append(kinds, "Certificate")
	}
	if r.opts.ExternalDNS.Enabled() {
		kinds = append(kinds, "DNSEndpoint")
	}
	for _, kind := range kinds {
		resources := r.c.Dynamic.Resource(gvrFor[kind]).Namespace(r.opts.SystemNamespace)
		objects, err := resources.List(ctx, metav1.ListOptions{LabelSelector: labels.Set(map[string]string{
			manifests.LabelInstallation: r.c.installation(), manifests.LabelRole: manifests.RoleEdge,
		}).String()})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, old := range objects.Items {
			space, app := old.GetLabels()[manifests.LabelSpace], old.GetLabels()[manifests.LabelApp]
			if !config.ValidSpaceName(space) || !config.ValidName(app) {
				continue
			}
			// Retry the annotation update after an interrupted cleanup, even if
			// the old Certificate has already been deleted.
			currentCertificate := kind == "Certificate" && old.GetName() == manifests.AppCertificateName(space, app)
			if old.GetName() != legacyAppTrafficName(kind, space, app) && !currentCertificate {
				continue
			}
			name := manifests.AppCertificateName(space, app)
			if kind == "DNSEndpoint" {
				name = manifests.AppDNSName(space, app)
			}
			source, err := resources.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				source = &old
			} else if err != nil {
				return err
			}
			if err := r.c.requireOwnedObject(kind, source.GetNamespace(), source.GetName(), source.GetLabels(), old.GetLabels()); err != nil {
				return err
			}
			spec, _, err := unstructured.NestedMap(source.Object, "spec")
			if err != nil {
				return err
			}
			next := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": source.GetAPIVersion(), "kind": kind, "spec": spec,
			}}
			next.SetNamespace(r.opts.SystemNamespace)
			next.SetName(name)
			next.SetLabels(source.GetLabels())
			next.SetAnnotations(source.GetAnnotations())
			if kind == "Certificate" {
				if err := unstructured.SetNestedField(next.Object, manifests.AppCertificateSecretName(space, app), "spec", "secretName"); err != nil {
					return err
				}
			}
			if err := r.c.Apply(ctx, []runtime.Object{next}); err != nil {
				return err
			}
		}
	}
	return nil
}
