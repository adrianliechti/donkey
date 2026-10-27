package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	"golang.org/x/crypto/acme/autocert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestGatewayACMECacheDoesNotReadPerRequest(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			client := fake.NewClientset()
			if !missing {
				_, err := client.CoreV1().Secrets("system").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: manifests.GatewayCertsSecret}, Data: map[string][]byte{"known": []byte("certificate")}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			adapter := &GatewayAdapter{Client: &Client{Clientset: client}, Namespace: "system"}
			cache := adapter.GatewayCertificateCache()
			client.ClearActions()
			for i := range 100 {
				_, _ = cache.Get(ctx, "known")
				if _, err := cache.Get(ctx, fmt.Sprintf("missing-%d", i)); !errors.Is(err, autocert.ErrCacheMiss) {
					t.Fatal("missing key did not return a cache miss")
				}
			}
			if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
				t.Fatalf("cache caused %d API calls", len(client.Actions()))
			}
			if err := cache.Put(ctx, "new", []byte("renewed")); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			for range 100 {
				value, err := cache.Get(ctx, "new")
				if err != nil || string(value) != "renewed" {
					t.Fatal("persisted certificate absent from cache")
				}
				value[0] = 'x'
			}
			if len(client.Actions()) != 0 {
				t.Fatal("reads after issuance accessed the API")
			}
			if err := cache.Delete(ctx, "new"); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			if _, err := cache.Get(ctx, "new"); !errors.Is(err, autocert.ErrCacheMiss) || len(client.Actions()) != 0 {
				t.Fatal("cache deletion did not take effect locally")
			}
		})
	}
}

func TestGatewayACMECacheBacksOffAPIOutage(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "secrets", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, errors.New("API unavailable") })
	cache := (&GatewayAdapter{Client: &Client{Clientset: client}, Namespace: "system"}).GatewayCertificateCache()
	for range 100 {
		if _, err := cache.Get(context.Background(), "unknown"); err == nil {
			t.Fatal("API failure was hidden")
		}
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("outage caused %d API requests", len(client.Actions()))
	}
}

func TestGatewaySnapshotContainsOwnedPolicies(t *testing.T) {
	space := manifests.SpaceMarker("team", "workloads")
	space.Labels[manifests.LabelInstallation] = "system"
	space.Data = map[string]string{"config.yaml": "viewers:\n  groups: [readers]\n"}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "web-route", Namespace: "workloads", Labels: manifests.Labels("team", "web", manifests.RoleRoute)}, Data: manifests.RouteData(config.GatewayRoute{Host: "web.example", Path: "/", Backend: "http://web:80"})}
	cm.Labels[manifests.LabelInstallation] = "system"
	foreign := cm.DeepCopy()
	foreign.Name = "foreign-route"
	foreign.Labels[manifests.LabelInstallation] = "other"
	client := fake.NewClientset(space, cm, foreign)
	adapter := &GatewayAdapter{Client: (&Client{Clientset: client}).ForInstallation("system", "workloads"), Namespace: "system"}
	snapshot, err := adapter.GatewaySnapshot(context.Background())
	if err != nil || len(snapshot.Routes) != 1 || snapshot.Routes[0].Space != "team" || len(snapshot.Spaces["team"].Viewers.Groups) != 1 {
		t.Fatalf("bad snapshot: %+v %v", snapshot, err)
	}
	space.Data["config.yaml"] = "unknownField: true\n"
	if _, err := client.CoreV1().ConfigMaps("workloads").Update(context.Background(), space, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.GatewaySnapshot(context.Background()); err == nil {
		t.Fatal("invalid policy did not fail closed")
	}
}

func TestRouteFromConfigMap(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				manifests.LabelSpace: "acme",
				manifests.LabelApp:   "shop",
			},
			Annotations: map[string]string{
				"donkey.local/backend":   "http://shop.donkey-acme.svc.cluster.local:80",
				"donkey.local/host":      "shop.example.com",
				"donkey.local/path":      "/api",
				"donkey.local/login":     "required",
				manifests.AnnotationMode: config.AppModeAPI,
				manifests.AnnotationCORS: config.CORSAllowAll,
				"donkey.local/access":    `{"users":["ada@example.com"]}`,
			},
		},
	}
	rt, ok := routeFromConfigMap(cm)
	if !ok {
		t.Fatal("route with backend annotation should be picked up")
	}
	if rt.Host != "shop.example.com" || rt.Path != "/api" || !rt.Login || rt.Mode != config.AppModeAPI || rt.CORS != config.CORSAllowAll {
		t.Errorf("unexpected route: %+v", rt)
	}
	if rt.Space != "acme" || rt.App != "shop" {
		t.Errorf("route telemetry scope = %q/%q", rt.Space, rt.App)
	}

	// No backend annotation → not the gateway's route (an unrelated
	// ConfigMap that happens to carry donkey labels).
	plain := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"donkey.local/host": "donkey.example.com"},
		},
	}
	if _, ok := routeFromConfigMap(plain); ok {
		t.Error("routes without a backend annotation are not ours to serve")
	}

	// Scope is mandatory: without it the route would work while gateway
	// request and view telemetry disappeared silently.
	unscoped := cm.DeepCopy()
	delete(unscoped.Labels, manifests.LabelApp)
	if _, ok := routeFromConfigMap(*unscoped); ok {
		t.Error("routes without a valid logical telemetry scope must not be served")
	}
	system := unscoped.DeepCopy()
	delete(system.Labels, manifests.LabelSpace)
	// Standard manager metadata is not routing authority; only Donkey identity
	// may distinguish the intentional unscoped system route.
	system.Labels[manifests.LabelManagedBy] = "Helm"
	system.Labels[manifests.LabelRole] = manifests.SystemRole(manifests.ServerName)
	system.Annotations[manifests.AnnotationToken] = manifests.TokenForward
	if route, ok := routeFromConfigMap(*system); !ok || !route.Token || route.Space != "" || route.App != "" {
		t.Errorf("the managed console route must remain system-scoped, got %+v, ok=%v", route, ok)
	}
	spoofedToken := cm.DeepCopy()
	spoofedToken.Annotations[manifests.AnnotationToken] = manifests.TokenForward
	if route, ok := routeFromConfigMap(*spoofedToken); !ok || route.Token {
		t.Errorf("a tenant route must not opt into admin-token forwarding, got %+v, ok=%v", route, ok)
	}

	// A non-http backend must be refused, not proxied.
	bad := cm.DeepCopy()
	bad.Annotations["donkey.local/backend"] = "https://evil.example.com"
	if _, ok := routeFromConfigMap(*bad); ok {
		t.Error("only in-cluster plain-http backends are proxied")
	}
}

func TestManagedCertificatesLoadOnlyOwnedGatewaySecrets(t *testing.T) {
	owned := tlsSecret(t, "shop.example.com", map[string]string{
		manifests.LabelInstallation: "donkey-system",
		manifests.LabelRole:         manifests.RoleEdge,
	})
	owned.Name, owned.Namespace = "shop-tls", "donkey-system"
	foreign := tlsSecret(t, "evil.example.com", map[string]string{
		manifests.LabelInstallation: "another-donkey",
		manifests.LabelRole:         manifests.RoleEdge,
	})
	foreign.Name, foreign.Namespace = "evil-tls", "donkey-system"
	unrelated := tlsSecret(t, "config.example.com", map[string]string{
		manifests.LabelInstallation: "donkey-system",
		manifests.LabelRole:         manifests.RoleConfig,
	})
	unrelated.Name, unrelated.Namespace = "unrelated-tls", "donkey-system"

	adapter := &GatewayAdapter{Client: &Client{Clientset: fake.NewSimpleClientset(owned, foreign, unrelated)}, Namespace: "donkey-system"}
	records, err := adapter.GatewayCertificates(context.Background())
	if err != nil || len(records) != 1 || records[0].Name != "donkey-system/shop-tls" {
		t.Fatalf("unexpected managed certificates: %+v, %v", records, err)
	}

}

func tlsSecret(t *testing.T, host string, labels map[string]string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Data: map[string][]byte{
			"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}
}
