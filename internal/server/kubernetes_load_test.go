package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// BenchmarkPortalKubernetesAPI measures the Kubernetes reads made by the real
// HTTP handlers and auth middleware for one fleet refresh (/fleet).
// The frontend repeats this every 10 seconds in each active portal tab.
// Fixtures are current, exposed, stateless apps with one running pod each.
// Fake clients count operations; timings do not represent API server latency.
// Reads/s is demand at the configured interval, before latency or throttling.
//
// Run without a cluster:
// go test ./internal/server -run '^$' -bench '^Benchmark(Portal|ResourceSampling)KubernetesAPI$' -benchtime=1x
func BenchmarkPortalKubernetesAPI(b *testing.B) {
	for _, shared := range []bool{false, true} {
		for _, member := range []bool{false, true} {
			for _, size := range []struct{ apps, spaces int }{{1, 1}, {10, 1}, {100, 10}} {
				mode, role := "per-space", "admin"
				if shared {
					mode = "shared"
				}
				if member {
					role = "developer"
				}
				for _, cached := range []bool{false, true} {
					cache := "cold"
					if cached {
						cache = "cached"
					}
					b.Run(fmt.Sprintf("%s/%s/apps=%d/spaces=%d/%s", mode, role, size.apps, size.spaces, cache), func(b *testing.B) {
						f := newKubernetesLoadFixture(b, shared, member, size.apps, size.spaces)
						if cached {
							var fleet kubernetesFleetResponse
							f.get(b, "/v1/fleet", &fleet)
							f.takeReads(b)
						}
						var reads int
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if !cached {
								f.runtime.InvalidateAppSummaries()
							}
							var fleet kubernetesFleetResponse
							f.get(b, "/v1/fleet", &fleet)
							if len(fleet.Apps) != size.apps {
								b.Fatalf("returned %d apps, want %d", len(fleet.Apps), size.apps)
							}
							if len(fleet.Spaces) != size.spaces {
								b.Fatalf("returned %d spaces, want %d", len(fleet.Spaces), size.spaces)
							}
							reads += f.takeReads(b)
						}
						b.StopTimer()
						perPoll := float64(reads) / float64(b.N)
						b.ReportMetric(perPoll, "kube-reads/poll")
						b.ReportMetric(perPoll/10, "requested-reads/s")
					})
				}
			}
		}
	}
}

// Sampling runs even when the portal is closed. Its request count should be
// assessed separately from the per-tab fleet work above.
func BenchmarkResourceSamplingKubernetesAPI(b *testing.B) {
	for _, shared := range []bool{false, true} {
		mode := "per-space"
		if shared {
			mode = "shared"
		}
		for _, apps := range []int{10, 100} {
			b.Run(fmt.Sprintf("%s/apps=%d", mode, apps), func(b *testing.B) {
				f := newKubernetesLoadFixture(b, shared, false, apps, 1)
				var reads int
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					samples, err := f.runtime.ResourceSamples(context.Background())
					if err != nil {
						b.Fatal(err)
					}
					if len(samples) != apps {
						b.Fatalf("returned %d samples, want %d", len(samples), apps)
					}
					reads += f.takeReads(b)
				}
				b.StopTimer()
				b.ReportMetric(float64(reads)/float64(b.N), "kube-reads/sample")
			})
		}
	}
}

type kubernetesLoadFixture struct {
	runtime *kubernetes.Runtime
	typed   *fake.Clientset
	dynamic *dynamicfake.FakeDynamicClient
	handler http.Handler
	token   string
}

type kubernetesFleetResponse struct {
	Apps []AppListEntry `json:"apps"`
	spaceListResponse
}

func newKubernetesLoadFixture(b testing.TB, shared, member bool, apps, spaces int) *kubernetesLoadFixture {
	b.Helper()
	const system = manifests.DefaultSystemNamespace
	placement := manifests.Placement{NamespacePrefix: "donkey-"}
	if shared {
		placement.SharedNamespace = "donkey-apps"
	}
	encode := func(value any) string {
		data, err := config.EncodeYAML(value)
		if err != nil {
			b.Fatal(err)
		}
		return string(data)
	}
	labels := func(values map[string]string) map[string]string {
		values[manifests.LabelInstallation] = system
		return values
	}
	objects := []k8sruntime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: manifests.SystemConfigMapName, Namespace: system,
			Labels: labels(manifests.SystemLabels(manifests.ServerName))},
		Data: map[string]string{"platform.yaml": encode(testPlatformConfig())},
	}}
	for i := 0; i < spaces; i++ {
		space := fmt.Sprintf("team-%d", i)
		ns := placement.Namespace(space)
		if !shared {
			namespace := manifests.Namespace(space, ns)
			namespace.Labels = labels(namespace.Labels)
			objects = append(objects, namespace)
		}
		marker := manifests.SpaceMarker(space, ns)
		marker.Labels = labels(marker.Labels)
		marker.Data = map[string]string{"config.yaml": encode(config.SpaceConfig{
			Developers: config.AccessConfig{Users: []string{"dev@example.com"}},
			Domains:    []string{"example.com"},
		})}
		objects = append(objects, marker)
	}
	var metrics []k8sruntime.Object
	for i := 0; i < apps; i++ {
		space, app := fmt.Sprintf("team-%d", i%spaces), fmt.Sprintf("app-%d", i)
		ns, name := placement.Namespace(space), placement.Name(space, app)
		workloadLabels := labels(manifests.Labels(space, app, manifests.RoleWorkload))
		objects = append(objects, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: workloadLabels},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: workloadLabels},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: manifests.WebContainerName, Image: "example/app:v1"}},
				}},
			},
		}, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: ns, Labels: workloadLabels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: manifests.WebContainerName}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
		route := manifests.AppRoute(manifests.Input{
			App: &config.AppConfig{Name: app}, Space: space, Namespace: ns, ObjectName: name,
			Domain: fmt.Sprintf("%s.%s.example.com", app, space),
		}).(*corev1.ConfigMap)
		route.Labels = labels(route.Labels)
		objects = append(objects, route)
		metrics = append(metrics, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metrics.k8s.io/v1", "kind": "PodMetrics",
			"metadata":  map[string]any{"name": name + "-pod", "namespace": ns, "labels": stringMapValues(workloadLabels)},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"containers": []any{map[string]any{
				"name": manifests.WebContainerName, "usage": map[string]any{"cpu": "100m", "memory": "64Mi"},
			}},
		}})
	}
	metricGVR := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1", Resource: "pods"}
	f := &kubernetesLoadFixture{
		typed: fake.NewClientset(objects...),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(),
			map[schema.GroupVersionResource]string{
				metricGVR: "PodMetricsList",
			}),
	}
	// PodMetrics is served as "pods", which the fake cannot infer from Kind.
	for _, object := range metrics {
		metric := object.(*unstructured.Unstructured)
		if _, err := f.dynamic.Resource(metricGVR).Namespace(metric.GetNamespace()).Create(
			context.Background(), metric, metav1.CreateOptions{},
		); err != nil {
			b.Fatal(err)
		}
	}
	f.dynamic.ClearActions()
	f.runtime = kubernetes.NewRuntime(&kubernetes.Client{Clientset: f.typed, Dynamic: f.dynamic}, kubernetes.Options{
		SystemNamespace: system, Placement: placement, Domain: "example.com",
	})
	f.handler = (&Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: f.runtime}).routes()
	email := "u1@example.com"
	if member {
		email = "dev@example.com"
	}
	var err error
	f.token, err = identity.SealToken(testSessionKey, identity.Identity{
		Subject: email, Email: email, Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// Dynamic fake clients filter PodMetrics objects themselves, while Metrics
// Server applies the selector to source pods. Mirror those labels in fixtures.
func stringMapValues(labels map[string]string) map[string]any {
	values := make(map[string]any, len(labels))
	for key, value := range labels {
		values[key] = value
	}
	return values
}

func (f *kubernetesLoadFixture) get(b testing.TB, path string, result any) {
	b.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Access-Token", f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		b.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), result); err != nil {
		b.Fatal(err)
	}
}

func (f *kubernetesLoadFixture) takeReads(b testing.TB) int {
	b.Helper()
	var reads int
	for _, actions := range [][]k8stesting.Action{f.typed.Actions(), f.dynamic.Actions()} {
		for _, action := range actions {
			if action.GetVerb() != "get" && action.GetVerb() != "list" {
				b.Fatalf("unexpected mutation: %s %s", action.GetVerb(), action.GetResource().Resource)
			}
			reads++
		}
	}
	f.typed.ClearActions()
	f.dynamic.ClearActions()
	return reads
}
