package kubernetes

import (
	"context"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// HasDefaultStorageClass reports whether the cluster can provision volumes
// without naming a class — what app storage and provisioned addons rely on.
func (c *Client) HasDefaultStorageClass(ctx context.Context) (bool, error) {
	scs, err := c.Clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, sc := range scs.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return true, nil
		}
	}
	return false, nil
}

// PVC names cannot be changed in place. Continue using an existing volume
// after renaming its workload, including on later deploys and in file browsing.
func (c *Client) dataClaim(ctx context.Context, namespace, name string, identity map[string]string) (*corev1.PersistentVolumeClaim, error) {
	pvcs := c.Clientset.CoreV1().PersistentVolumeClaims(namespace)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	space := identity[manifests.LabelSpace]
	logical := identity[manifests.LabelApp]
	if logical == "" {
		logical = identity[manifests.LabelAddon]
	}
	if apierrors.IsNotFound(err) && space != "" && logical != "" && name == space+"-"+logical+"-data" {
		pvc, err = pvcs.Get(ctx, space+"--"+logical+"-data", metav1.GetOptions{})
	}
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject("PersistentVolumeClaim", namespace, pvc.Name, pvc.Labels, identity); err != nil {
		return nil, err
	}
	return pvc, nil
}

func (c *Client) preserveDataClaims(ctx context.Context, objects []runtime.Object) error {
	names := map[string]string{}
	for _, object := range objects {
		pvc, ok := object.(*corev1.PersistentVolumeClaim)
		if !ok {
			continue
		}
		stored, err := c.dataClaim(ctx, pvc.Namespace, pvc.Name, pvc.Labels)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		names[pvc.Namespace+"/"+pvc.Name] = stored.Name
		pvc.Name = stored.Name
	}
	for _, object := range objects {
		if dep, ok := object.(*appsv1.Deployment); ok {
			for i := range dep.Spec.Template.Spec.Volumes {
				if claim := dep.Spec.Template.Spec.Volumes[i].PersistentVolumeClaim; claim != nil {
					if name := names[dep.Namespace+"/"+claim.ClaimName]; name != "" {
						claim.ClaimName = name
					}
				}
			}
		}
	}
	return nil
}
