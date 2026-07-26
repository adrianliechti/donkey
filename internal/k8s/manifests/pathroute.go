package manifests

import (
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ComponentRoute marks the extra routes an app claims on another app's
// address, so they can be found and retracted when they go away.
const ComponentRoute = "route"

var routeSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// PathRouteName is the object name for one shared-address route. It has to be
// derived from the target and the path, because an app may claim several.
func PathRouteName(app, targetApp, path string) string {
	slug := strings.Trim(routeSlugRE.ReplaceAllString(strings.ToLower(path), "-"), "-")
	if slug == "" {
		slug = "root"
	}
	return app + "-on-" + targetApp + "-" + slug
}

// PathRoute puts one path prefix of another app's hostname onto this app.
//
// Gateway API matches the most specific path first, so a "/api" route always
// wins over the target app's own "/" — no ordering to configure. And because
// each route carries its own login setting, the two can differ: a
// machine-facing API can stay open under a path of a private frontend.
func PathRoute(in Input, r RouteTarget) *gatewayv1.HTTPRoute {
	return PathRouteNamed(in, PathRouteName(in.App.Name, r.App, r.Path), r)
}

// PathRouteNamed renders a shared-address route under an explicit object
// name — what the console uses to re-render a route that already exists on
// the cluster (whose name encodes a target app the cluster no longer spells
// out).
func PathRouteNamed(in Input, name string, r RouteTarget) *gatewayv1.HTTPRoute {
	app := in.App
	gwNamespace := gatewayv1.Namespace(in.GatewayNamespace)
	section := gatewayv1.SectionName(HTTPSListenerName)
	noTimeout := gatewayv1.Duration("0s")
	prefix := gatewayv1.PathMatchPathPrefix

	route := &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, app.Name, ComponentRoute),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:        gatewayv1.ObjectName(in.GatewayName),
					Namespace:   &gwNamespace,
					SectionName: &section,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(r.Host)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{Type: &prefix, Value: &r.Path},
				}},
				Timeouts:    &gatewayv1.HTTPRouteTimeouts{Request: &noTimeout},
				BackendRefs: []gatewayv1.HTTPBackendRef{routeBackend(in)},
			}},
		},
	}
	route.Annotations = routeAnnotations(in)
	return route
}
