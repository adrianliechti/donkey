package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

// DefaultAppGatewayNameTemplate keeps the external Gateway aligned with the
// app's Kubernetes Deployment name unless an installation has a stricter
// platform naming contract.
const DefaultAppGatewayNameTemplate = "$name"

// AppGatewayName expands an app Gateway name template. The hash is stable for
// an installation, space and app so a platform-compatible opaque name remains
// unchanged across deploys and server restarts.
func AppGatewayName(template, namespace, space, app, name string) (string, error) {
	if template == "" {
		template = DefaultAppGatewayNameTemplate
	}

	digest := sha256.Sum256([]byte(namespace + "\x00" + space + "\x00" + app))
	values := map[string]string{
		"namespace": namespace,
		"space":     space,
		"app":       app,
		"name":      name,
		"hash":      fmt.Sprintf("%x", digest[:5]),
	}
	unknown := ""
	rendered := os.Expand(template, func(parameter string) string {
		value, ok := values[parameter]
		if !ok && unknown == "" {
			unknown = parameter
		}
		return value
	})
	if unknown != "" {
		return "", fmt.Errorf("gateway.appGateway.nameTemplate %q uses unknown parameter $%s — available parameters are $name, $namespace, $space, $app and $hash", template, unknown)
	}
	if rendered == "" || len(rendered) > 253 {
		return "", fmt.Errorf("gateway.appGateway.nameTemplate %q renders an invalid Kubernetes object name %q", template, rendered)
	}
	for _, label := range strings.Split(rendered, ".") {
		if !ValidNamespace(label) {
			return "", fmt.Errorf("gateway.appGateway.nameTemplate %q renders an invalid Kubernetes object name %q", template, rendered)
		}
	}
	return rendered, nil
}
