package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// migrateRouteConfigs keeps old annotations readable while the gateway rolls
// forward. New writers and readers use YAML data entries exclusively.
func (r *Runtime) migrateRouteConfigs(ctx context.Context) error {
	routes, err := r.c.ListManagedRoutes(ctx)
	if err != nil {
		return err
	}
	for _, cm := range routes {
		route, err := manifests.ReadRoute(&cm)
		if err != nil {
			return fmt.Errorf("ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
		name := cm.Name
		app := cm.Labels[manifests.LabelApp]
		space := cm.Labels[manifests.LabelSpace]
		if app != "" && space != "" {
			owner := r.name(space, app)
			switch cm.Labels[manifests.LabelRole] {
			case manifests.RoleRoute:
				name = manifests.AppRouteName(owner)
			case manifests.RolePathRoute:
				for _, oldOwner := range []string{owner, space + "--" + app} {
					if rest, old := strings.CutPrefix(cm.Name, oldOwner+"-on-"); old {
						name = owner + "-route-" + rest
					} else if rest, old := strings.CutPrefix(cm.Name, oldOwner+"-route-"); old {
						name = owner + "-route-" + rest
					}
				}
			}
		}
		_, current := cm.Data[manifests.RouteConfigKey]
		if r.opts.Placement.Shared() && app != "" && space != "" {
			oldBackend := "http://" + space + "--" + app + "." + cm.Namespace + ".svc"
			if rest, old := strings.CutPrefix(route.Backend, oldBackend); old && (rest == "" || strings.HasPrefix(rest, ":")) {
				route.Backend = "http://" + r.name(space, app) + "." + cm.Namespace + ".svc" + rest
				current = false
			}
		}
		if current && name == cm.Name {
			continue
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if !current {
			for key, value := range manifests.RouteData(route) {
				cm.Data[key] = value
			}
		}
		cms := r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace)
		if name == cm.Name {
			if _, err := cms.Update(ctx, &cm, metav1.UpdateOptions{FieldManager: "donkey-config-migration"}); err != nil {
				return err
			}
			continue
		}
		fresh := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: cm.Namespace, Labels: maps.Clone(cm.Labels), Annotations: maps.Clone(cm.Annotations),
		}, Data: maps.Clone(cm.Data)}
		created, err := cms.Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey-config-migration"})
		if apierrors.IsAlreadyExists(err) {
			created, err = cms.Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return err
		}
		if err := r.c.requireOwnedObject("ConfigMap", cm.Namespace, name, created.Labels, cm.Labels); err != nil {
			return err
		}
		if err := r.c.deleteConfigMap(ctx, &cm); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) deleteConfigMap(ctx context.Context, cm *corev1.ConfigMap) error {
	opts := metav1.DeleteOptions{}
	if cm.UID != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &cm.UID}
	}
	err := c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, opts)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// copySecret changes a name without generating or replacing any credential.
// A destination that already exists wins, after its ownership is verified.
func (c *Client) copySecret(ctx context.Context, old *corev1.Secret, name string) error {
	secrets := c.Clientset.CoreV1().Secrets(old.Namespace)
	fresh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: old.Namespace, Labels: maps.Clone(old.Labels), Annotations: maps.Clone(old.Annotations),
	}, Type: old.Type, Data: maps.Clone(old.Data)}
	stored, err := secrets.Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey-config-migration"})
	if apierrors.IsAlreadyExists(err) {
		stored, err = secrets.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return err
	}
	return c.requireOwnedObject("Secret", old.Namespace, name, stored.Labels, old.Labels)
}

// migrateServiceSecrets updates references in Deployment templates before old
// binding Secrets are removed. Their environment variable names stay intact.
func (r *Runtime) migrateServiceSecrets(ctx context.Context) error {
	for _, namespace := range r.c.sweep() {
		secrets, err := r.c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: manifests.LabelInstallation + "=" + r.c.installation(),
		})
		if err != nil {
			return err
		}
		renames := map[string]map[string]string{}
		for _, secret := range secrets.Items {
			if !r.c.ours(secret.Labels) {
				continue
			}
			name := r.migratedSecretName(&secret)
			if name == "" || name == secret.Name {
				continue
			}
			if err := r.c.copySecret(ctx, &secret, name); err != nil {
				return err
			}
			if secret.Labels[manifests.LabelRole] == manifests.RoleEdge {
				if err := r.renameCertificateSecret(ctx, &secret, name); err != nil {
					return err
				}
			}
			if renames[secret.Namespace] == nil {
				renames[secret.Namespace] = map[string]string{}
			}
			renames[secret.Namespace][secret.Name] = name
		}
		for ns, names := range renames {
			deps, err := r.c.Clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
				LabelSelector: manifests.LabelInstallation + "=" + r.c.installation(),
			})
			if err != nil {
				return err
			}
			for _, dep := range deps.Items {
				if !r.c.ours(dep.Labels) || !renameSecretReferences(&dep.Spec.Template.Spec, names) {
					continue
				}
				if _, err := r.c.Clientset.AppsV1().Deployments(ns).Update(ctx, &dep, metav1.UpdateOptions{FieldManager: "donkey-config-migration"}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func renameSecretReferences(spec *corev1.PodSpec, names map[string]string) bool {
	changed := false
	rename := func(name *string) {
		if next := names[*name]; next != "" && next != *name {
			*name = next
			changed = true
		}
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			for j := range containers[i].EnvFrom {
				if ref := containers[i].EnvFrom[j].SecretRef; ref != nil {
					rename(&ref.Name)
				}
			}
			for j := range containers[i].Env {
				if source := containers[i].Env[j].ValueFrom; source != nil && source.SecretKeyRef != nil {
					rename(&source.SecretKeyRef.Name)
				}
			}
		}
	}
	for i := range spec.ImagePullSecrets {
		rename(&spec.ImagePullSecrets[i].Name)
	}
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Secret != nil {
			rename(&volume.Secret.SecretName)
		}
		if volume.Projected != nil {
			for j := range volume.Projected.Sources {
				if ref := volume.Projected.Sources[j].Secret; ref != nil {
					rename(&ref.Name)
				}
			}
		}
	}
	return changed
}

// Only exact legacy names in the expected namespace are eligible for migration.
func (r *Runtime) migratedSecretName(secret *corev1.Secret) string {
	space, app := secret.Labels[manifests.LabelSpace], secret.Labels[manifests.LabelApp]
	if !config.ValidName(space) || (app != "" && !config.ValidName(app)) {
		return ""
	}
	switch secret.Labels[manifests.LabelRole] {
	case manifests.RoleBinding:
		service := secret.Labels[manifests.LabelAddon]
		if !config.ValidName(service) || secret.Namespace != r.ns(space) {
			return ""
		}
		owner := r.sharedOwner(space)
		if app != "" {
			owner = r.name(space, app)
		}
		legacyOwner := owner
		if r.opts.Placement.Shared() {
			logical := app
			if logical == "" {
				logical = addons.SharedOwner
			}
			legacyOwner = space + "--" + logical
		}
		if secret.Name == owner+"-addon-"+service || secret.Name == legacyOwner+"-addon-"+service || secret.Name == addons.SecretName(legacyOwner, service) && legacyOwner != owner {
			return addons.SecretName(owner, service)
		}
	case manifests.RoleConfig, manifests.RoleRegistryAuth:
		logical := app
		if logical == "" {
			logical = secret.Labels[manifests.LabelAddon]
		}
		if r.opts.Placement.Shared() && config.ValidName(logical) && secret.Namespace == r.ns(space) {
			for _, suffix := range []string{"-env", "-telemetry", "-registry"} {
				if secret.Name == space+"--"+logical+suffix {
					return r.name(space, logical) + suffix
				}
			}
		}
	case manifests.RoleEdge:
		if app != "" && secret.Namespace == r.opts.SystemNamespace && secret.Name == manifests.AppCertificateName(space, app)+"-tls" {
			return manifests.AppCertificateSecretName(space, app)
		}
	}
	return ""
}

func (r *Runtime) renameCertificateSecret(ctx context.Context, old *corev1.Secret, name string) error {
	certificates := r.c.Dynamic.Resource(gvrFor["Certificate"]).Namespace(old.Namespace)
	space, app := old.Labels[manifests.LabelSpace], old.Labels[manifests.LabelApp]
	cert, err := certificates.Get(ctx, manifests.AppCertificateName(space, app), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.c.requireOwnedObject("Certificate", old.Namespace, cert.GetName(), cert.GetLabels(), manifests.Labels(space, app, manifests.RoleEdge)); err != nil {
		return err
	}
	current, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	if current != old.Name {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": cert.GetUID()},
		"spec":     map[string]any{"secretName": name},
	})
	if err != nil {
		return err
	}
	_, err = certificates.Patch(ctx, cert.GetName(), types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey"})
	return err
}

// FinishConfigMigration runs after the new control plane is ready. A binding
// still referenced by a Pod is retained until its Deployment finishes rolling.
// Re-running an install or deploying that app retries cleanup safely.
func (r *Runtime) FinishConfigMigration(ctx context.Context) error {
	for _, namespace := range r.c.sweep() {
		secrets, err := r.c.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: manifests.LabelInstallation + "=" + r.c.installation(),
		})
		if err != nil {
			return err
		}
		for _, old := range secrets.Items {
			name := r.migratedSecretName(&old)
			if !r.c.ours(old.Labels) || name == "" {
				continue
			}
			stored, err := r.c.Clientset.CoreV1().Secrets(old.Namespace).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := r.c.requireOwnedObject("Secret", old.Namespace, name, stored.Labels, old.Labels); err != nil {
				return err
			}
			// Include operator-managed Pods too: no live consumer loses a Secret.
			pods, err := r.c.Clientset.CoreV1().Pods(old.Namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			inUse := false
			for _, pod := range pods.Items {
				if renameSecretReferences(pod.Spec.DeepCopy(), map[string]string{old.Name: name}) {
					inUse = true
					break
				}
			}
			if inUse {
				continue
			}
			if old.Labels[manifests.LabelRole] == manifests.RoleEdge {
				cert, err := r.c.Dynamic.Resource(gvrFor["Certificate"]).Namespace(old.Namespace).
					Get(ctx, manifests.AppCertificateName(old.Labels[manifests.LabelSpace], old.Labels[manifests.LabelApp]), metav1.GetOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				if err == nil {
					current, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
					if current == old.Name {
						continue
					}
				}
			}
			opts := metav1.DeleteOptions{}
			if old.UID != "" {
				opts.Preconditions = &metav1.Preconditions{UID: &old.UID}
			}
			if err := r.c.Clientset.CoreV1().Secrets(old.Namespace).Delete(ctx, old.Name, opts); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	routes, err := r.c.ListManagedRoutes(ctx)
	if err != nil {
		return err
	}
	for _, cm := range routes {
		if _, current := cm.Data[manifests.RouteConfigKey]; !current {
			continue
		}
		if _, err := manifests.ReadRoute(&cm); err != nil {
			return err
		}
		annotations := map[string]any{}
		for _, key := range []string{manifests.AnnotationBackend, manifests.AnnotationHost, manifests.AnnotationPath,
			manifests.AnnotationLogin, manifests.AnnotationMode, manifests.AnnotationCORS, manifests.AnnotationAccess,
			manifests.AnnotationStripPrefix, manifests.AnnotationToken} {
			if _, exists := cm.Annotations[key]; exists {
				annotations[key] = nil
			}
		}
		if len(annotations) == 0 {
			continue
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": cm.UID, "annotations": annotations}})
		if err != nil {
			return err
		}
		if _, err := r.c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch,
			metav1.PatchOptions{FieldManager: "donkey-config-migration"}); err != nil {
			return err
		}
	}
	return nil
}
