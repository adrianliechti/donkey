package manifests

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// A route is one entry of the donkey gateway's routing table: which hostname
// (and path) reaches which app, whether visitors sign in first, and who may
// pass. It is stored as a ConfigMap carrying annotations — the gateway is
// donkey's own edge (it terminates TLS and proxies straight to the app's
// Service), so no ingress vocabulary is needed, just the table.
const (
	// AnnotationBackend names the app Service the donkey gateway forwards
	// to. Every route carries it — it is what makes a ConfigMap a route.
	AnnotationBackend = "donkey.dev/backend"
	// AnnotationHost is the hostname the route serves.
	AnnotationHost = "donkey.dev/host"
	// AnnotationPath is the route's path prefix; "/" for an app's own address.
	AnnotationPath = "donkey.dev/path"
	// AnnotationLogin marks a route whose visitors must sign in first. Its
	// absence is what "public" means at the edge.
	AnnotationLogin = "donkey.dev/login"
	// AnnotationAccess carries the app's access rules (AccessConfig as JSON)
	// for the gateway to enforce. No secrets — emails and group names only.
	AnnotationAccess = "donkey.dev/access"

	// LoginRequired is AnnotationLogin's one meaningful value.
	LoginRequired = "required"
)

// GatewayRef points at donkey's own edge proxy — every app route's real
// backend. Auth says whether an IdP is configured; without one nothing can
// be protected, but traffic still routes.
type GatewayRef struct {
	Service   string
	Namespace string
	Auth      bool
}

// protected says visitors of this app's routes must sign in.
func protected(in Input) bool {
	return !in.App.Public && in.Gateway != nil && in.Gateway.Auth
}

// backendURL is where the gateway forwards this app's traffic: the app's
// Service, on the stable in-cluster port.
func backendURL(in Input) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:80", in.App.Name, in.Namespace)
}

// routeAnnotations is the gateway's routing table entry: where the app
// really is, which hostname and path it serves, whether to demand a login,
// and who may pass.
func routeAnnotations(in Input, host, path string) map[string]string {
	ann := map[string]string{
		AnnotationBackend: backendURL(in),
		AnnotationHost:    host,
		AnnotationPath:    path,
	}
	if protected(in) {
		ann[AnnotationLogin] = LoginRequired
		if !in.App.Access.Empty() {
			if b, err := json.Marshal(in.App.Access); err == nil {
				ann[AnnotationAccess] = string(b)
			}
		}
	}
	return ann
}

// routeConfigMap wraps a routing table entry in the object that stores it.
func routeConfigMap(name, namespace string, labels, annotations map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
	}
}

// appRoute exposes the app on its hostname through the donkey gateway. The
// gateway serves https only — the login cookies are Secure-only, so serving
// apps over plain http was never right; its http listener redirects.
func appRoute(in Input) *corev1.ConfigMap {
	return routeConfigMap(in.App.Name, in.Namespace,
		Labels(in.Space, in.App.Name, ComponentWeb),
		routeAnnotations(in, in.Domain, "/"))
}

// The console changes exposure without a deploy, so it re-renders the
// network objects piecewise — these exports are its building blocks.

// AppRoute is the app's own-hostname route.
func AppRoute(in Input) runtime.Object { return appRoute(in) }
