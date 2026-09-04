package manifests

import (
	"encoding/json"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

// buildEnv is the installation's build configuration as donkey-server reads
// it. The mirror hosts let its BuildKit session authenticate private-CA token
// endpoints; the proxy is spelled the way every tool already expects (see
// proxyEnv).
func buildEnv(b config.InstallBuild) []corev1.EnvVar {
	var env []corev1.EnvVar
	if len(b.RegistryMirrors) > 0 {
		env = append(env, corev1.EnvVar{Name: "DONKEY_REGISTRY_MIRRORS", Value: registryMirrorsJSON(b.RegistryMirrors)})
	}
	// Only what was configured: an installation that says nothing about
	// builds renders the deployment it always did.
	for _, e := range []corev1.EnvVar{
		{Name: "DONKEY_BUILDER_VERIFY", Value: verifyValue(b.BuildSettings)},
		{Name: "DONKEY_BUILDER_VERIFY_SECONDS", Value: positiveOrEmpty(b.VerifySeconds)},
		{Name: "DONKEY_BUILDER_ATTEMPTS", Value: positiveOrEmpty(b.Attempts)},
		{Name: "DONKEY_BUILDER_PROVENANCE", Value: b.Provenance},
		{Name: "DONKEY_BUILDER_SBOM", Value: trueOrEmpty(b.SBOM)},
	} {
		if e.Value != "" {
			env = append(env, e)
		}
	}
	if len(b.Env) > 0 {
		// A map travels as JSON, the way DONKEY_ADMINS does — but out of the
		// Secret, because these values are routinely credentialed URLs.
		env = append(env, secretEnvVar("DONKEY_BUILD_ENV", "build-env"))
	}
	return env
}

func registryMirrorsJSON(mirrors map[string]string) string {
	raw, err := json.Marshal(mirrors)
	if err != nil {
		return ""
	}
	return string(raw)
}

// verifyValue renders the tri-state startup-verification setting: empty
// leaves the server on its own default (on), so only an explicit choice
// travels.
func verifyValue(b config.BuildSettings) string {
	if b.Verify == nil {
		return ""
	}
	return strconv.FormatBool(*b.Verify)
}

// positiveOrEmpty renders an optional numeric setting; zero means "unset",
// which the server's accessor turns into the default.
func positiveOrEmpty(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// buildEnvJSON is the build environment as donkey-server reads it.
func buildEnvJSON(b config.InstallBuild) string {
	if len(b.Env) == 0 {
		return ""
	}
	raw, err := json.Marshal(b.Env)
	if err != nil {
		return ""
	}
	return string(raw)
}

// secretEnvVar reads one value out of the installation's system Secret.
func secretEnvVar(name, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: SystemConfigSecretName},
			Key:                  key,
		},
	}}
}

// inClusterNoProxy is what must never be sent to a forward proxy: donkey's
// own components reach each other over cluster addresses, and no corporate
// proxy can route those. It is appended to whatever the installation
// configured, because forgetting it does not degrade anything — it takes the
// control plane off the air.
//
// The Kubernetes API is not in this list because it cannot be: a pod reaches
// it at a bare service IP whose range differs per cluster. That one is
// handled where it belongs, on the client (see kubernetes.NewInCluster).
const inClusterNoProxy = "localhost,127.0.0.1,.svc,.svc.cluster.local,.cluster.local"

// proxyEnv is the forward proxy in the spelling Go's HTTP client, curl and
// git already read — donkey configures no tool for it individually.
//
// registryHost is donkey's own registry, excluded by name: it is routinely a
// bare service name or an address inside the cluster that no domain suffix
// covers, and sending pushes to a corporate proxy would break every build.
func proxyEnv(p config.InstallProxy, registryHost string) []corev1.EnvVar {
	var env []corev1.EnvVar
	if p.HTTP != "" || p.HTTPS != "" {
		exclude := inClusterNoProxy
		if registryHost != "" {
			exclude += "," + registryHost
		}
		p.NoProxy = strings.Trim(strings.TrimSpace(p.NoProxy)+","+exclude, ",")
	}
	// Both spellings: some tools read only lowercase, and Go's client ignores
	// HTTP_PROXY for CGI-safety. The two that can carry credentials come out
	// of the Secret; NO_PROXY is a host list and stays plain.
	secret := func(name, key, value string) {
		if value == "" {
			return
		}
		env = append(env, secretEnvVar(name, key), secretEnvVar(strings.ToLower(name), key))
	}
	secret("HTTP_PROXY", "http-proxy", p.HTTP)
	secret("HTTPS_PROXY", "https-proxy", p.HTTPS)
	if p.NoProxy != "" {
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: p.NoProxy},
			corev1.EnvVar{Name: "no_proxy", Value: p.NoProxy},
		)
	}
	return env
}

// trueOrEmpty renders an opt-in boolean the way this env block travels:
// only what was configured.
func trueOrEmpty(v bool) string {
	if v {
		return "true"
	}
	return ""
}
