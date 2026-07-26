package manifests

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
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
// The gateway matches the most specific path first, so a "/api" route always
// wins over the target app's own "/" — no ordering to configure. And because
// each route carries its own login setting, the two can differ: a
// machine-facing API can stay open under a path of a private frontend.
func PathRoute(in Input, r RouteTarget) *corev1.ConfigMap {
	return PathRouteNamed(in, PathRouteName(in.App.Name, r.App, r.Path), r)
}

// PathRouteNamed renders a shared-address route under an explicit object
// name — what the console uses to re-render a route that already exists on
// the cluster (whose name encodes a target app the cluster no longer spells
// out).
func PathRouteNamed(in Input, name string, r RouteTarget) *corev1.ConfigMap {
	return routeConfigMap(name, in.Namespace,
		Labels(in.Space, in.App.Name, ComponentRoute),
		routeAnnotations(in, r.Host, r.Path))
}
