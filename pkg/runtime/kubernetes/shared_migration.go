package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const migrationReplicas = "donkey.local/migration-replicas"

// Shared workloads previously used space--app. New Services keep the same
// selectors; old Services remain DNS aliases for stored connection strings.
// PVCs retain their names and data. A replacement Deployment starts only after
// its predecessor has drained, so both cannot write the same disk concurrently.
func (r *Runtime) migrateSharedWorkloadNames(ctx context.Context) error {
	if !r.opts.Placement.Shared() {
		return nil
	}
	ns := r.opts.Placement.SharedNamespace
	selector := metav1.ListOptions{LabelSelector: manifests.LabelInstallation + "=" + r.c.installation()}
	services, err := r.c.Clientset.CoreV1().Services(ns).List(ctx, selector)
	if err != nil {
		return err
	}
	for _, old := range services.Items {
		name := r.sharedWorkloadName(old.Name, old.Labels)
		if name == "" {
			continue
		}
		fresh := &corev1.Service{ObjectMeta: migrationMetadata(old.ObjectMeta, name), Spec: *old.Spec.DeepCopy()}
		fresh.Spec.ClusterIP, fresh.Spec.ClusterIPs = "", nil
		for i := range fresh.Spec.Ports {
			fresh.Spec.Ports[i].NodePort = 0
		}
		stored, err := r.c.Clientset.CoreV1().Services(ns).Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey"})
		if apierrors.IsAlreadyExists(err) {
			stored, err = r.c.Clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return err
		}
		if err := r.c.requireOwnedObject("Service", ns, name, stored.Labels, old.Labels); err != nil {
			return err
		}
	}
	if err := r.migrateServiceSecrets(ctx); err != nil {
		return err
	}
	secrets, err := r.c.Clientset.CoreV1().Secrets(ns).List(ctx, selector)
	if err != nil {
		return err
	}
	renames := map[string]string{}
	for _, secret := range secrets.Items {
		if r.c.ours(secret.Labels) {
			if name := r.migratedSecretName(&secret); name != "" {
				renames[secret.Name] = name
			}
		}
	}
	deps := r.c.Clientset.AppsV1().Deployments(ns)
	list, err := deps.List(ctx, selector)
	if err != nil {
		return err
	}
	for _, old := range list.Items {
		name := r.sharedWorkloadName(old.Name, old.Labels)
		if name == "" {
			continue
		}
		fresh := &appsv1.Deployment{ObjectMeta: migrationMetadata(old.ObjectMeta, name), Spec: *old.Spec.DeepCopy()}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		replicas := int32(1)
		if old.Spec.Replicas != nil {
			replicas = *old.Spec.Replicas
		}
		fresh.Annotations[migrationReplicas] = strconv.Itoa(int(replicas))
		fresh.Spec.Replicas = new(int32(0))
		renameSecretReferences(&fresh.Spec.Template.Spec, renames)
		stored, err := deps.Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey"})
		if apierrors.IsAlreadyExists(err) {
			stored, err = deps.Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return err
		}
		if err := r.c.requireOwnedObject("Deployment", ns, name, stored.Labels, old.Labels); err != nil {
			return err
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			dep, err := deps.Get(ctx, old.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if dep.UID != old.UID {
				return fmt.Errorf("Deployment %s changed during migration", old.Name)
			}
			dep.Spec.Replicas = new(int32(0))
			_, err = deps.Update(ctx, dep, metav1.UpdateOptions{FieldManager: "donkey"})
			return err
		}); err != nil {
			return err
		}
		if err := r.c.WaitForRollout(ctx, ns, old.Name, old.Labels); err != nil {
			return err
		}
		pods, err := r.c.Clientset.CoreV1().Pods(ns).List(ctx, selector)
		if err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if sameObjectIdentity(pod.Labels, old.Labels) && strings.HasPrefix(pod.Name, old.Name+"-") {
				if err := r.c.waitPodGone(ctx, ns, pod.Name); err != nil {
					return err
				}
			}
		}

		// Keep the zero-replica predecessor until the successor's desired
		// replica count is durable, so a retry can recover after interruption.
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			dep, err := deps.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if saved, found := dep.Annotations[migrationReplicas]; found {
				count, err := strconv.ParseInt(saved, 10, 32)
				if err != nil || count < 0 {
					return fmt.Errorf("invalid saved replica count on %s", name)
				}
				dep.Spec.Replicas = new(int32(count))
				delete(dep.Annotations, migrationReplicas)
				_, err = deps.Update(ctx, dep, metav1.UpdateOptions{FieldManager: "donkey"})
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		opts := metav1.DeleteOptions{}
		if old.UID != "" {
			opts.Preconditions = &metav1.Preconditions{UID: &old.UID}
		}
		if err := deps.Delete(ctx, old.Name, opts); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func migrationMetadata(old metav1.ObjectMeta, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: old.Namespace, Labels: maps.Clone(old.Labels), Annotations: maps.Clone(old.Annotations)}
}

func (r *Runtime) sharedWorkloadName(name string, labels map[string]string) string {
	if !r.c.ours(labels) || !config.ValidSpaceName(labels[manifests.LabelSpace]) {
		return ""
	}
	logical := labels[manifests.LabelApp]
	if labels[manifests.LabelRole] == manifests.RoleAddon {
		logical = labels[manifests.LabelAddon]
	} else if labels[manifests.LabelRole] != manifests.RoleWorkload {
		return ""
	}
	space := labels[manifests.LabelSpace]
	if config.ValidName(logical) && name == space+"--"+logical {
		return r.name(space, logical)
	}
	return ""
}
