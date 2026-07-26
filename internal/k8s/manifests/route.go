package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	// AnnotationBackend on an HTTPRoute names the app Service the donkey
	// gateway forwards to. Every app route carries it — the gateway is the
	// app edge, and this is where the traffic really goes.
	AnnotationBackend = "donkey.dev/backend"
	// AnnotationLogin marks a route whose visitors must sign in first. Its
	// absence is what "public" means at the edge.
	AnnotationLogin = "donkey.dev/login"
	// AnnotationAccess on an HTTPRoute carries the app's access rules
	// (AccessConfig as JSON) for the gateway to enforce. No secrets —
	// emails and group names only.
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

// routeAnnotations tells the gateway what to do with a route: where the app
// really is, whether to demand a login, and who may pass.
func routeAnnotations(in Input) map[string]string {
	if in.Gateway == nil {
		return nil
	}
	ann := map[string]string{AnnotationBackend: backendURL(in)}
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

// routeBackend is the one backendRef of an app route: the donkey gateway,
// across namespaces (the matching ReferenceGrant is rendered alongside).
// Without a gateway (tests, odd installs) the route falls back to the app's
// own Service.
func routeBackend(in Input) gatewayv1.HTTPBackendRef {
	port := gatewayv1.PortNumber(80)
	ref := gatewayv1.BackendObjectReference{
		Name: gatewayv1.ObjectName(in.App.Name),
		Port: &port,
	}
	if in.Gateway != nil {
		ns := gatewayv1.Namespace(in.Gateway.Namespace)
		ref.Name = gatewayv1.ObjectName(in.Gateway.Service)
		ref.Namespace = &ns
	}
	return gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}}
}

// httpRoute exposes the app on its hostname through the shared donkey
// Gateway (Gateway API — no Ingress). It attaches to the https listener only:
// the login gateway's cookies are Secure-only, so serving the app over plain
// http was never right — the shared redirect route sends port-80 visitors to
// https instead. Streaming responses (SSE, long downloads) must not be cut by
// Envoy's default request timeout, so it is disabled like on the API's route.
func httpRoute(in Input) *gatewayv1.HTTPRoute {
	app := in.App
	gwNamespace := gatewayv1.Namespace(in.GatewayNamespace)
	section := gatewayv1.SectionName(HTTPSListenerName)
	noTimeout := gatewayv1.Duration("0s")
	route := &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, app.Name, ComponentWeb),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:        gatewayv1.ObjectName(in.GatewayName),
					Namespace:   &gwNamespace,
					SectionName: &section,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(in.Domain)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Timeouts:    &gatewayv1.HTTPRouteTimeouts{Request: &noTimeout},
				BackendRefs: []gatewayv1.HTTPBackendRef{routeBackend(in)},
			}},
		},
	}
	route.Annotations = routeAnnotations(in)
	return route
}

// OIDCSecretName is the per-app client-secret Secret earlier donkey versions
// rendered for Envoy's SecurityPolicy. Login now lives in donkey-gateway;
// the name remains so deploys can retract the leftovers.
func OIDCSecretName(app string) string { return app + "-oidc" }

// GatewayGrantName is the ReferenceGrant that lets one app's routes point at
// the login gateway's Service across namespaces. Per app (grants are
// additive), next to the gateway, hashed for the same reason as AppCertName:
// dashes are legal inside space and app names.
func GatewayGrantName(space, app string) string {
	sum := sha256.Sum256([]byte(space + "/" + app))
	return fmt.Sprintf("donkey-gateway-%s-%s-%s", space, app, hex.EncodeToString(sum[:3]))
}

// gatewayGrant allows this app's HTTPRoutes to reference the login gateway's
// Service. It lives in the gateway's namespace — ReferenceGrants belong to
// the namespace being referenced, which is the point of them.
func gatewayGrant(in Input) *gatewayv1beta1.ReferenceGrant {
	return &gatewayv1beta1.ReferenceGrant{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1beta1", Kind: "ReferenceGrant"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayGrantName(in.Space, in.App.Name),
			Namespace: in.Gateway.Namespace,
			Labels:    Labels(in.Space, in.App.Name, ComponentWeb),
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{
				Group:     "gateway.networking.k8s.io",
				Kind:      "HTTPRoute",
				Namespace: gatewayv1beta1.Namespace(in.Namespace),
			}},
			To: []gatewayv1beta1.ReferenceGrantTo{{
				Kind: "Service",
				Name: ptrTo(gatewayv1beta1.ObjectName(in.Gateway.Service)),
			}},
		},
	}
}

func ptrTo[T any](v T) *T { return &v }

// The console changes exposure without a deploy, so it re-renders the
// network objects piecewise — these exports are its building blocks.

// AppRoute is the app's own-hostname route.
func AppRoute(in Input) runtime.Object { return httpRoute(in) }

// AppCertificateObject is the app's certificate (requires in.TLS.Issuer).
func AppCertificateObject(in Input) runtime.Object { return appCertificate(in) }

// GatewayGrant allows the app's routes to reference the donkey gateway.
func GatewayGrant(in Input) runtime.Object { return gatewayGrant(in) }
