package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const fleetCacheTTL = 5 * time.Second

type fleetCache struct {
	mu    sync.Mutex
	apps  []rt.AppSummary
	err   error
	until time.Time
}

var _ rt.AppSummaryProvider = (*Runtime)(nil)

// AppSummaries makes five lists per sweep namespace, regardless of app count.
// Only display values survive the read: no Secret data or management policies
// enter this cache. A lock coalesces concurrent portal refreshes, including the
// initial read, and serializes invalidation with an in-flight snapshot.
func (r *Runtime) AppSummaries(ctx context.Context) ([]rt.AppSummary, error) {
	r.fleet.mu.Lock()
	defer r.fleet.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !time.Now().Before(r.fleet.until) {
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		apps, err := r.readAppSummaries(readCtx)
		cancel()
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		r.fleet.apps, r.fleet.err = apps, err
		ttl := fleetCacheTTL
		if err != nil {
			// An API outage must not turn concurrent browser retries into
			// repeated full sweeps. Failed reads never serve stale status.
			ttl = time.Second
		}
		r.fleet.until = time.Now().Add(ttl)
	}
	return cloneAppSummaries(r.fleet.apps), r.fleet.err
}

func (r *Runtime) InvalidateAppSummaries() {
	r.fleet.mu.Lock()
	r.fleet.until = time.Time{}
	r.fleet.mu.Unlock()
}

func cloneAppSummaries(apps []rt.AppSummary) []rt.AppSummary {
	result := slices.Clone(apps)
	for i := range result {
		result[i].Status.Instances = slices.Clone(result[i].Status.Instances)
		result[i].Addons = slices.Clone(result[i].Addons)
	}
	return result
}

func (r *Runtime) readAppSummaries(ctx context.Context) ([]rt.AppSummary, error) {
	var result []rt.AppSummary
	installation := fmt.Sprintf("%s=%s", manifests.LabelInstallation, r.c.installation())
	selector := func(role string) metav1.ListOptions {
		return metav1.ListOptions{LabelSelector: installation + "," + manifests.LabelApp + "," + manifests.LabelRole + "=" + role}
	}
	for _, ns := range r.workloadNamespaces() {
		deps, err := r.c.Clientset.AppsV1().Deployments(ns).List(ctx, selector(manifests.RoleWorkload))
		if err != nil {
			return nil, fmt.Errorf("listing fleet deployments: %w", err)
		}
		if len(deps.Items) == 0 {
			continue
		}
		pods, err := r.c.Clientset.CoreV1().Pods(ns).List(ctx, selector(manifests.RoleWorkload))
		if err != nil {
			return nil, fmt.Errorf("listing fleet pods: %w", err)
		}
		routes, err := r.c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, selector(manifests.RoleRoute))
		if err != nil {
			return nil, fmt.Errorf("listing fleet routes: %w", err)
		}
		claims, err := r.c.Clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, selector(manifests.RoleStorage))
		if err != nil {
			return nil, fmt.Errorf("listing fleet storage: %w", err)
		}
		bindings, err := r.c.Clientset.CoreV1().Secrets(ns).List(ctx, selector(manifests.RoleBinding))
		if err != nil {
			return nil, fmt.Errorf("listing fleet bindings: %w", err)
		}

		podsByApp := map[rt.AppRef][]*corev1.Pod{}
		for i := range pods.Items {
			pod := &pods.Items[i]
			ref := rt.AppRef{Space: pod.Labels[manifests.LabelSpace], App: pod.Labels[manifests.LabelApp]}
			if r.fleetObjectOwned(pod.ObjectMeta, ref, manifests.RoleWorkload) {
				podsByApp[ref] = append(podsByApp[ref], pod)
			}
		}
		routesByName := map[string]*corev1.ConfigMap{}
		for i := range routes.Items {
			cm := &routes.Items[i]
			routesByName[cm.Namespace+"/"+cm.Name] = cm
		}
		claimsByName := map[string]*corev1.PersistentVolumeClaim{}
		for i := range claims.Items {
			pvc := &claims.Items[i]
			claimsByName[pvc.Namespace+"/"+pvc.Name] = pvc
		}
		addonsByApp := map[rt.AppRef][]string{}
		for _, sec := range bindings.Items {
			ref := rt.AppRef{Space: sec.Labels[manifests.LabelSpace], App: sec.Labels[manifests.LabelApp]}
			addon := sec.Labels[manifests.LabelAddon]
			identity := manifests.AddonLabels(ref.Space, addon, manifests.RoleBinding)
			identity[manifests.LabelApp] = ref.App
			if ref.Space != "" && ref.App != "" && addon != "" && sec.Namespace == r.ns(ref.Space) &&
				r.c.requireOwnedObject("Secret", sec.Namespace, sec.Name, sec.Labels, identity) == nil {
				addonsByApp[ref] = append(addonsByApp[ref], addon)
			}
		}
		for i := range deps.Items {
			dep := &deps.Items[i]
			ref := rt.AppRef{Space: dep.Labels[manifests.LabelSpace], App: dep.Labels[manifests.LabelApp]}
			name := r.name(ref.Space, ref.App)
			if dep.Name != name || !r.fleetObjectOwned(dep.ObjectMeta, ref, manifests.RoleWorkload) {
				continue
			}
			st := deploymentStatus(dep)
			if dep.Spec.Selector != nil {
				podSelector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
				for _, pod := range podsByApp[ref] {
					if podSelector.Matches(labels.Set(pod.Labels)) {
						addPodStatus(st, pod)
					}
				}
			}
			cm := routesByName[dep.Namespace+"/"+manifests.AppRouteName(name)]
			if cm == nil {
				cm = routesByName[dep.Namespace+"/"+name] // legacy route
			}
			if cm != nil && r.fleetObjectOwned(cm.ObjectMeta, ref, manifests.RoleRoute) {
				addRouteStatus(st, cm)
			}
			pvc := claimsByName[dep.Namespace+"/"+manifests.DataClaimName(name)]
			if pvc == nil && r.opts.Placement.Shared() {
				pvc = claimsByName[dep.Namespace+"/"+ref.Space+"--"+ref.App+"-data"]
			}
			addons := addonsByApp[ref]
			slices.Sort(addons)
			result = append(result, rt.AppSummary{
				AppRef: ref, Status: *st, Addons: slices.Compact(addons),
				HasStorage: pvc != nil && r.fleetObjectOwned(pvc.ObjectMeta, ref, manifests.RoleStorage),
			})
		}
	}
	return result, nil
}

func (r *Runtime) fleetObjectOwned(meta metav1.ObjectMeta, ref rt.AppRef, role string) bool {
	return ref.Space != "" && ref.App != "" && meta.Namespace == r.ns(ref.Space) &&
		sameObjectIdentity(meta.Labels, r.c.expectedObjectIdentity(manifests.Labels(ref.Space, ref.App, role)))
}
