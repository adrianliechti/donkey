package manifests

import (
	"encoding/json"
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// A route is configuration in data["route.yaml"]. Identity and ownership stay
// in labels; annotations are reserved for metadata. The old annotations below
// remain readable during upgrades.
const (
	// AnnotationBackend names the app Service the donkey gateway forwards
	// to in the legacy annotation format.
	AnnotationBackend = "donkey.local/backend"
	// AnnotationHost is the hostname the route serves.
	AnnotationHost = "donkey.local/host"
	// AnnotationPath is the route's path prefix; "/" for an app's own address.
	AnnotationPath        = "donkey.local/path"
	AnnotationStripPrefix = "donkey.local/strip-prefix"
	// AnnotationLogin marks a route whose visitors must sign in first. Its
	// absence is what "public" means at the edge.
	AnnotationLogin = "donkey.local/login"
	// AnnotationMode selects browser sessions or OAuth for APIs.
	// Its absence is the default web mode.
	AnnotationMode = "donkey.local/mode"
	// AnnotationCORS lets the edge answer preflight requests and make all
	// responses browser-readable from any origin.
	AnnotationCORS = "donkey.local/cors"
	// AnnotationAccess is legacy app membership, read only during migration.
	AnnotationAccess = "donkey.local/access"

	// LoginRequired is AnnotationLogin's one meaningful value.
	LoginRequired = "required"
	CORSAllowAll  = config.CORSAllowAll

	// AnnotationToken marks the one route whose backend verifies identity
	// itself: the gateway forwards a short-lived sealed token
	// (X-Access-Token) minted from the login. Only donkey's own console/API
	// route carries it — an app must never receive a credential its code
	// could replay against the API.
	AnnotationToken = "donkey.local/token"
	// TokenForward is AnnotationToken's one meaningful value.
	TokenForward = "forward"
)

// protected says visitors of this app's routes must sign in.
func protected(in Input) bool {
	return !in.App.Public
}

// backendURL is where the gateway forwards this app's traffic: the app's
// Service, on the stable in-cluster port.
//
// ".svc" and no further: the cluster domain is a per-cluster setting
// ("cluster.local" is only the default) and every pod's resolv.conf carries
// "svc.<domain>" in its search list, so the short form resolves on clusters
// where a hardcoded fully-qualified name would not.
func backendURL(in Input) string {
	return fmt.Sprintf("http://%s.%s.svc:80", in.Name(), in.Namespace)
}

const (
	RouteConfigKey = "route.yaml"
	RouteAccessKey = "access.yaml"
)

// IsRoute includes old annotation-based entries while an upgrade is running.
func IsRoute(cm *corev1.ConfigMap) bool {
	_, found := cm.Data[RouteConfigKey]
	return found || cm.Annotations[AnnotationBackend] != ""
}

// ReadRoute accepts the current configuration and the legacy annotation format.
// A malformed current record never falls back to older, potentially wider rules.
func ReadRoute(cm *corev1.ConfigMap) (config.GatewayRoute, error) {
	var route config.GatewayRoute
	if raw, found := cm.Data[RouteConfigKey]; found {
		if err := config.DecodeYAML([]byte(raw), &route); err != nil {
			return route, fmt.Errorf("cannot read route configuration: %w", err)
		}
	} else {
		ann := cm.Annotations
		route = config.GatewayRoute{
			Host: ann[AnnotationHost], Path: ann[AnnotationPath], Backend: ann[AnnotationBackend],
			Login: ann[AnnotationLogin] == LoginRequired, Mode: ann[AnnotationMode], CORS: ann[AnnotationCORS],
			Token: ann[AnnotationToken] == TokenForward, StripPrefix: ann[AnnotationStripPrefix] == "true",
		}
		if raw := ann[AnnotationAccess]; raw != "" {
			var access config.AccessConfig
			if err := json.Unmarshal([]byte(raw), &access); err != nil {
				return route, fmt.Errorf("cannot read route access: %w", err)
			}
			route.Access = &access
		}
	}
	if raw, found := cm.Data[RouteAccessKey]; found {
		var access config.AccessConfig
		if err := config.DecodeYAML([]byte(raw), &access); err != nil {
			return route, fmt.Errorf("cannot read route access: %w", err)
		}
		route.Access = &access
	}
	if route.Access != nil {
		if err := route.Access.Validate(); err != nil {
			return route, err
		}
	}
	if route.Path == "" {
		route.Path = "/"
	}
	return route, nil
}

func routeSettings(in Input, host, path string) config.GatewayRoute {
	route := config.GatewayRoute{Host: host, Path: path, Backend: backendURL(in), Login: protected(in)}
	if in.App.IsAPI() {
		route.Mode = config.AppModeAPI
	}
	if in.App.AllowsAllOrigins() {
		route.CORS = CORSAllowAll
	}
	route.Analytics = config.RouteAnalytics(in.App)
	return route
}

// RouteData writes routing fields. The separate legacy access key is retained
// only while old app membership is being moved to the space.
func RouteData(route config.GatewayRoute) map[string]string {
	data := map[string]string{}
	if route.Access != nil {
		raw, _ := config.EncodeYAML(route.Access)
		data[RouteAccessKey] = string(raw)
		route.Access = nil
	}
	raw, _ := config.EncodeYAML(route)
	data[RouteConfigKey] = string(raw)
	return data
}

func routeConfigMap(name, namespace string, labels map[string]string, route config.GatewayRoute) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       RouteData(route),
	}
}

// appRoute exposes an app through the gateway's HTTPS endpoint.
func appRoute(in Input) *corev1.ConfigMap {
	return routeConfigMap(AppRouteName(in.Name()), in.Namespace,
		Labels(in.Space, in.App.Name, RoleRoute), routeSettings(in, in.Domain, "/"))
}

// AppRoute is the app's own-hostname route.
func AppRoute(in Input) runtime.Object { return appRoute(in) }

// AppRouteName identifies the app’s routing configuration.
func AppRouteName(app string) string { return app + "-route" }
