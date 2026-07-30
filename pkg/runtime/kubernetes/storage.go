package kubernetes

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
