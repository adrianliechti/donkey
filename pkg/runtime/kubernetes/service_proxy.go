package kubernetes

import (
	"context"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// ServiceProxyAccess uses current pod and binding records, never forwarded
// headers or an IP-to-space cache that could outlive an IP's previous owner.
func (r *Runtime) ServiceProxyAccess(ctx context.Context, sourceIP, offering string) (bool, error) {
	ip, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return false, nil
	}
	ip = ip.Unmap()
	ns := r.opts.Placement.SharedNamespace // empty means NamespaceAll
	selector := labels.Set(map[string]string{manifests.LabelInstallation: r.opts.SystemNamespace, manifests.LabelRole: manifests.RoleWorkload}).String()
	pods, err := r.c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	var caller *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.HostNetwork || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		space, app := pod.Labels[manifests.LabelSpace], pod.Labels[manifests.LabelApp]
		if config.ValidateSpaceName(space) != nil || config.ValidateAppName(app) != nil || pod.Namespace != r.ns(space) {
			continue
		}
		if r.c.requireOwnedObject("Pod", pod.Namespace, pod.Name, pod.Labels, manifests.Labels(space, app, manifests.RoleWorkload)) != nil {
			continue
		}
		ips := append([]corev1.PodIP{{IP: pod.Status.PodIP}}, pod.Status.PodIPs...)
		for _, value := range ips {
			address, err := netip.ParseAddr(value.IP)
			if err == nil && address.Unmap() == ip {
				if caller != nil && caller != pod {
					return false, nil
				} // ambiguous identity
				caller = pod
				break
			}
		}
	}
	if caller == nil {
		return false, nil
	}
	return r.ServiceProxySpaceAccess(ctx, caller.Labels[manifests.LabelSpace], offering)
}

func (r *Runtime) ServiceProxySpaceAccess(ctx context.Context, space, offering string) (bool, error) {
	if config.ValidateSpaceName(space) != nil {
		return false, nil
	}
	selector := labels.Set(map[string]string{manifests.LabelInstallation: r.opts.SystemNamespace, manifests.LabelSpace: space, manifests.LabelRole: manifests.RoleBinding}).String()
	bindings, err := r.c.Clientset.CoreV1().Secrets(r.ns(space)).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for _, binding := range bindings.Items {
		name := binding.Labels[manifests.LabelAddon]
		if name == "" || r.c.requireOwnedObject("Secret", binding.Namespace, binding.Name, binding.Labels, manifests.AddonLabels(space, name, manifests.RoleBinding)) != nil {
			continue
		}
		if string(binding.Data[addons.ProxyEnvKey(offering)]) == addons.ProxyBindingURL(r.serviceProxyURL(), offering) {
			return true, nil
		}
	}
	return false, nil
}
