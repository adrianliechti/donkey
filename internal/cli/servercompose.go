package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
	runtimedocker "github.com/adrianliechti/donkey/pkg/runtime/docker"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const (
	composeNetworkSubnet = "172.31.250.0/24"
	composePackagesIP    = "172.31.250.10"
)

type composeFile struct {
	Services map[string]composeService `json:"services"`
	Networks map[string]composeNetwork `json:"networks"`
	Volumes  map[string]composeVolume  `json:"volumes"`
}

type composeService struct {
	Image         string                       `json:"image"`
	ContainerName string                       `json:"container_name"`
	Command       []string                     `json:"command,omitempty"`
	Environment   map[string]string            `json:"environment,omitempty"`
	Volumes       []string                     `json:"volumes,omitempty"`
	Ports         []string                     `json:"ports,omitempty"`
	Privileged    bool                         `json:"privileged,omitempty"`
	Restart       string                       `json:"restart,omitempty"`
	Healthcheck   *composeHealthcheck          `json:"healthcheck,omitempty"`
	DependsOn     map[string]composeDependency `json:"depends_on,omitempty"`
	Networks      any                          `json:"networks,omitempty"`
	Labels        map[string]string            `json:"labels,omitempty"`
}

type composeHealthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval"`
	Timeout  string   `json:"timeout"`
	Retries  int      `json:"retries"`
}

type composeDependency struct {
	Condition string `json:"condition"`
}

type composeNetwork struct {
	Name   string             `json:"name"`
	IPAM   composeNetworkIPAM `json:"ipam"`
	Labels map[string]string  `json:"labels,omitempty"`
}

type composeNetworkIPAM struct {
	Config []composeSubnet `json:"config"`
}

type composeSubnet struct {
	Subnet string `json:"subnet"`
}

type composeServiceNetwork struct {
	IPv4Address string `json:"ipv4_address,omitempty"`
}

type composeVolume struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

func renderDockerBundle(settings *config.InstallFile) (map[string]bundleFile, error) {
	renderSettings, err := cloneInstallSettings(settings)
	if err != nil {
		return nil, err
	}
	bundledRegistry := renderSettings.Registry.URL == ""
	if bundledRegistry {
		renderSettings.Registry.URL = dockerRegistryName + ":5000/donkey"
		renderSettings.Registry.Insecure = true
	}
	if err := validateDockerSettings(renderSettings); err != nil {
		return nil, err
	}
	if err := validateRenderedPackageCredentials(renderSettings); err != nil {
		return nil, err
	}

	files, features, err := dockerComposeFiles(renderSettings, bundledRegistry)
	if err != nil {
		return nil, err
	}
	files["secrets.env.example"] = bundleFile{Data: []byte(composeSecretsExample), Mode: 0o600}
	files["up.sh"] = bundleFile{Data: []byte(renderComposeUpScript(renderSettings, features, bundledRegistry)), Mode: 0o755}
	files["README.md"] = bundleFile{Data: []byte(renderComposeReadme(renderSettings, bundledRegistry))}
	return files, nil
}

type composeSecretFeatures struct {
	RegistryCredentials bool
	PackagePassword     bool
	BuildEnvironment    bool
	BuilderInstructions bool
	HTTPForwardProxy    bool
	HTTPSForwardProxy   bool
}

func dockerComposeFiles(settings *config.InstallFile, bundledRegistry bool) (map[string]bundleFile, composeSecretFeatures, error) {
	features := composeSecretFeatures{
		RegistryCredentials: settings.Registry.Username != "",
		PackagePassword:     settings.Build.Packages.NeedsPassword(),
		BuildEnvironment:    len(settings.Build.Env) > 0,
		BuilderInstructions: settings.Build.Instructions != "",
		HTTPForwardProxy:    settings.Build.Proxy.HTTP != "",
		HTTPSForwardProxy:   settings.Build.Proxy.HTTPS != "",
	}

	buildkitImage := settings.BuildkitImage
	if buildkitImage == "" {
		buildkitImage = config.DefaultBuildkitImage
	}
	buildkitConfig := manifests.BuildkitConfig{
		RegistryHost:      settings.Registry.Host(),
		RegistryPlainHTTP: settings.Registry.Insecure,
		Mirrors:           settings.Build.RegistryMirrors,
	}
	files := map[string]bundleFile{}
	caMount := ""
	if settings.Registry.CAFile != "" {
		ca, err := os.ReadFile(settings.Registry.CAFile)
		if err != nil {
			return nil, features, fmt.Errorf("reading the registry's certificate authority: %w", err)
		}
		files["registry-ca.crt"] = bundleFile{Data: ca}
		buildkitConfig.RegistryCAFile = dockerRegistryCAPath
		caMount = "./registry-ca.crt:" + dockerRegistryCAPath + ":ro"
	}
	files["buildkitd.toml"] = bundleFile{Data: []byte(manifests.BuildkitTOML(buildkitConfig))}

	serverHost := settings.ServerHost
	if serverHost == "" {
		serverHost = settings.Domain
	}
	tlsMode := settings.TLS.EffectiveMode()
	clientID := settings.Auth.ClientID
	clientSecret := "${DONKEY_OIDC_CLIENT_SECRET:-}"
	if clientID == "" {
		clientID = pendingOIDC
		clientSecret = pendingOIDC
	}

	packagesEnv := map[string]string{
		"DONKEY_LISTEN":                    ":8090",
		"DONKEY_PACKAGES_NPM_URL":          settings.Build.Packages.NPMUpstream(),
		"DONKEY_PACKAGES_PYPI_URL":         settings.Build.Packages.PyPIUpstream(),
		"DONKEY_PACKAGES_GO_URL":           settings.Build.Packages.GoUpstream(),
		"DONKEY_PACKAGES_MAVEN_URL":        settings.Build.Packages.MavenUpstream(),
		"DONKEY_PACKAGES_NUGET_URL":        settings.Build.Packages.NuGetUpstream(),
		"DONKEY_PACKAGES_RESOURCE_HOSTS":   strings.Join(settings.Build.Packages.ResourceHosts, ","),
		"DONKEY_PACKAGES_CREDENTIAL_HOSTS": strings.Join(settings.Build.Packages.CredentialHosts, ","),
		"DONKEY_PACKAGES_USERNAME":         settings.Build.Packages.Username,
	}
	if features.PackagePassword {
		packagesEnv["DONKEY_PACKAGES_PASSWORD"] = "${DONKEY_PACKAGES_PASSWORD:-}"
	}
	packagesVolumes := []string{}
	if caMount != "" {
		packagesEnv["DONKEY_PACKAGES_CA_FILE"] = dockerRegistryCAPath
		packagesVolumes = append(packagesVolumes, caMount)
	}
	if dir := settings.Build.Packages.DockerConfigDir; dir != "" {
		packagesEnv["DONKEY_PACKAGES_DOCKER_CONFIG_DIR"] = dockerPackagesAuth
		packagesEnv["DONKEY_PACKAGES_DOCKER_CONFIG_HOST"] = settings.Build.Packages.DockerConfigHost
		packagesVolumes = append(packagesVolumes, dir+":"+dockerPackagesAuth+":ro")
	}
	addComposeProxyEnv(packagesEnv, settings.Build.Proxy)

	buildkitEnv := map[string]string{}
	addComposeProxyEnv(buildkitEnv, settings.Build.Proxy)
	buildkitVolumes := []string{
		"./buildkitd.toml:/etc/buildkit/buildkitd.toml:ro",
		"buildkit:/var/lib/buildkit",
	}
	if caMount != "" {
		buildkitVolumes = append(buildkitVolumes, caMount)
	}

	serverEnv := map[string]string{
		"DONKEY_RUNTIME":                 "docker",
		"DONKEY_STATE_DIR":               dockerDataDir,
		"DONKEY_LISTEN":                  ":8088",
		"DONKEY_DOMAIN":                  settings.Domain,
		"DONKEY_SERVER_HOST":             serverHost,
		"DONKEY_REGISTRY_URL":            settings.Registry.URL,
		"DONKEY_REGISTRY_USERNAME":       settings.Registry.Username,
		"DONKEY_REGISTRY_INSECURE":       strconv.FormatBool(settings.Registry.Insecure),
		"DONKEY_REGISTRY_IMMUTABLE":      strconv.FormatBool(settings.Registry.Immutable),
		"DONKEY_BUILDKIT_ADDR":           "tcp://" + dockerBuildkitName + ":1234",
		"DONKEY_PACKAGES_URL":            "http://${DONKEY_PACKAGES_IP:-" + composePackagesIP + "}:8090",
		"DONKEY_AI_URL":                  "http://" + dockerAIName + ":8089",
		"DONKEY_TELEMETRY_URL":           "http://" + dockerTelemetryName + ":9090",
		"DONKEY_TELEMETRY_OTLP_ENDPOINT": "http://" + dockerTelemetryName + ":4318",
		"DONKEY_DEFAULT_PLATFORM":        settings.DefaultPlatform,
		"DONKEY_POSTGRES_IMAGE":          settings.AddonImages.Postgres,
		"DONKEY_RUSTFS_IMAGE":            settings.AddonImages.RustFS,
		"DONKEY_OIDC_ISSUER":             settings.Auth.Issuer,
		"DONKEY_OIDC_CLIENT_ID":          clientID,
		"DONKEY_OIDC_CLIENT_SECRET":      clientSecret,
		"DONKEY_SESSION_KEY":             "${DONKEY_SESSION_KEY:-}",
		"DONKEY_TELEMETRY_KEY":           "${DONKEY_TELEMETRY_KEY:-}",
		"DONKEY_PLATFORM_ADMINS":         adminsJSONString(&settings.Platform.Admins),
		"DONKEY_AI_MODEL":                settings.AI.Model,
		"OPENAI_BASE_URL":                settings.AI.BaseURL,
		"OPENAI_API_KEY":                 "${OPENAI_API_KEY:-}",
	}
	if len(settings.Build.RegistryMirrors) > 0 {
		serverEnv["DONKEY_REGISTRY_MIRRORS"] = registryMirrorsJSON(settings.Build.RegistryMirrors)
	}
	if bundledRegistry {
		serverEnv["DONKEY_PULL_REGISTRY"] = "127.0.0.1:" + dockerRegistryHostPort
	}
	if features.RegistryCredentials {
		serverEnv["DONKEY_REGISTRY_PASSWORD"] = "${DONKEY_REGISTRY_PASSWORD:-}"
	}
	if features.BuilderInstructions {
		serverEnv["DONKEY_BUILDER_INSTRUCTIONS"] = "${DONKEY_BUILDER_INSTRUCTIONS:-}"
	}
	if settings.Build.Verify != nil {
		serverEnv["DONKEY_BUILDER_VERIFY"] = strconv.FormatBool(*settings.Build.Verify)
	}
	if settings.Build.VerifySeconds > 0 {
		serverEnv["DONKEY_BUILDER_VERIFY_SECONDS"] = strconv.Itoa(settings.Build.VerifySeconds)
	}
	if settings.Build.Attempts > 0 {
		serverEnv["DONKEY_BUILDER_ATTEMPTS"] = strconv.Itoa(settings.Build.Attempts)
	}
	if settings.Build.Provenance != "" {
		serverEnv["DONKEY_BUILDER_PROVENANCE"] = settings.Build.Provenance
	}
	if settings.Build.SBOM {
		serverEnv["DONKEY_BUILDER_SBOM"] = "true"
	}
	if features.BuildEnvironment {
		serverEnv["DONKEY_BUILD_ENV"] = "${DONKEY_BUILD_ENV:-}"
	}
	addComposeProxyEnv(serverEnv, settings.Build.Proxy)
	serverVolumes := []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"data:" + dockerDataDir,
	}
	if caMount != "" {
		serverEnv["DONKEY_REGISTRY_CA_FILE"] = dockerRegistryCAPath
		serverVolumes = append(serverVolumes, caMount)
	}
	if dir := settings.Registry.DockerConfigDir; dir != "" {
		serverEnv["DONKEY_REGISTRY_CONFIG_DIR"] = dockerRegistryAuthPath
		serverVolumes = append(serverVolumes, dir+":"+dockerRegistryAuthPath+":ro")
	}

	gatewayEnv := map[string]string{
		"DONKEY_RUNTIME":                 "docker",
		"DONKEY_STATE_DIR":               dockerDataDir,
		"DONKEY_LISTEN":                  ":8090",
		"DONKEY_TLS_LISTEN":              ":8443",
		"DONKEY_HEALTH_LISTEN":           ":8091",
		"DONKEY_DOMAIN":                  settings.Domain,
		"DONKEY_SERVER_HOST":             serverHost,
		"DONKEY_TLS_MODE":                tlsMode,
		"DONKEY_ACME_EMAIL":              settings.TLS.Email,
		"DONKEY_ACME_DIRECTORY":          settings.TLS.Directory,
		"DONKEY_OIDC_ISSUER":             settings.Auth.Issuer,
		"DONKEY_OIDC_CLIENT_ID":          clientID,
		"DONKEY_OIDC_CLIENT_SECRET":      clientSecret,
		"DONKEY_SESSION_KEY":             "${DONKEY_SESSION_KEY:-}",
		"DONKEY_TELEMETRY_KEY":           "${DONKEY_TELEMETRY_KEY:-}",
		"DONKEY_TELEMETRY_OTLP_ENDPOINT": "http://" + dockerTelemetryName + ":4318",
	}
	addComposeProxyEnv(gatewayEnv, settings.Build.Proxy)

	aiEnv := map[string]string{
		"DONKEY_RUNTIME":   "docker",
		"DONKEY_STATE_DIR": dockerDataDir,
		"DONKEY_LISTEN":    ":8089",
		"OPENAI_BASE_URL":  settings.AI.BaseURL,
		"OPENAI_API_KEY":   "${OPENAI_API_KEY:-}",
	}
	addComposeProxyEnv(aiEnv, settings.Build.Proxy)

	healthy := func() *composeHealthcheck {
		return &composeHealthcheck{Interval: "2s", Timeout: "6s", Retries: 3}
	}
	packagesHealth := healthy()
	packagesHealth.Test = []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8090/readyz"}
	telemetryHealth := healthy()
	telemetryHealth.Test = []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9090/readyz"}

	services := map[string]composeService{
		"packages": {
			Image: settings.Image, ContainerName: dockerPackagesName, Command: []string{"packages"},
			Environment: packagesEnv, Volumes: packagesVolumes, Restart: "unless-stopped",
			Healthcheck: packagesHealth,
			Networks: map[string]composeServiceNetwork{
				"donkey": {IPv4Address: "${DONKEY_PACKAGES_IP:-" + composePackagesIP + "}"},
			},
		},
		"buildkit": {
			Image: buildkitImage, ContainerName: dockerBuildkitName,
			Command:     []string{"--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml"},
			Environment: buildkitEnv, Volumes: buildkitVolumes, Privileged: true, Restart: "unless-stopped",
			Networks: []string{"donkey"},
		},
		"telemetry": {
			Image: settings.Image, ContainerName: dockerTelemetryName, Command: []string{"telemetry"},
			Environment: map[string]string{
				"DONKEY_TELEMETRY_LISTEN":     ":4318",
				"DONKEY_TELEMETRY_API_LISTEN": ":9090",
				"DONKEY_TELEMETRY_DB":         "/var/lib/donkey/telemetry.duckdb",
				"DONKEY_TELEMETRY_KEY":        "${DONKEY_TELEMETRY_KEY:-}",
			},
			Volumes: []string{"telemetry:/var/lib/donkey"}, Restart: "unless-stopped",
			Healthcheck: telemetryHealth, Networks: []string{"donkey"},
		},
		"ai": {
			Image: settings.Image, ContainerName: dockerAIName, Command: []string{"ai"},
			Environment: aiEnv, Volumes: []string{"data:" + dockerDataDir + ":ro"},
			Restart: "unless-stopped", Networks: []string{"donkey"},
		},
		"server": {
			Image: settings.Image, ContainerName: dockerServerName, Environment: serverEnv,
			Volumes: serverVolumes, Ports: []string{"127.0.0.1:8088:8088"}, Restart: "unless-stopped",
			DependsOn: map[string]composeDependency{
				"packages":  {Condition: "service_healthy"},
				"telemetry": {Condition: "service_healthy"},
				"buildkit":  {Condition: "service_started"},
				"ai":        {Condition: "service_started"},
			},
			Networks: []string{"donkey"},
		},
		"gateway": {
			Image: settings.Image, ContainerName: dockerGatewayName, Command: []string{"gateway"},
			Environment: gatewayEnv, Volumes: []string{"data:" + dockerDataDir},
			Ports: []string{"80:8090", "443:8443"}, Restart: "unless-stopped",
			DependsOn: map[string]composeDependency{
				"server":    {Condition: "service_started"},
				"telemetry": {Condition: "service_healthy"},
			},
			Networks: []string{"donkey"},
		},
	}
	if bundledRegistry {
		services["registry"] = composeService{
			Image: dockerRegistryImage, ContainerName: dockerRegistryName,
			Volumes: []string{"registry:/var/lib/registry"},
			Ports:   []string{"127.0.0.1:" + dockerRegistryHostPort + ":5000"},
			Restart: "unless-stopped", Networks: []string{"donkey"},
		}
		buildkit := services["buildkit"]
		buildkit.DependsOn = map[string]composeDependency{"registry": {Condition: "service_started"}}
		services["buildkit"] = buildkit
		server := services["server"]
		server.DependsOn["registry"] = composeDependency{Condition: "service_started"}
		services["server"] = server
	}
	// Label every service with the role the direct installer and the
	// bundle's own preflight expect for its container name. Deriving the
	// role from the compose service key instead is how a bundle ends up
	// refusing the very containers it created.
	roles := map[string]string{}
	for _, item := range dockerInstallContainers(bundledRegistry) {
		roles[item.name] = item.role
	}
	for key, service := range services {
		role, ok := roles[service.ContainerName]
		if !ok {
			return nil, features, fmt.Errorf("compose service %s has no install role for container %s", key, service.ContainerName)
		}
		service.Labels = runtimedocker.SystemLabels(role)
		services[key] = service
	}

	compose := composeFile{
		Services: services,
		Networks: map[string]composeNetwork{
			"donkey": {
				Name:   dockerNetwork,
				IPAM:   composeNetworkIPAM{Config: []composeSubnet{{Subnet: "${DONKEY_NETWORK_SUBNET:-" + composeNetworkSubnet + "}"}}},
				Labels: runtimedocker.SystemLabels(runtimedocker.RoleNetwork),
			},
		},
		Volumes: map[string]composeVolume{
			"data":      {Name: dockerDataVolume, Labels: runtimedocker.SystemLabels(runtimedocker.RoleStateStorage)},
			"buildkit":  {Name: dockerBuildkitVolume, Labels: runtimedocker.SystemLabels(runtimedocker.RoleBuildkitStorage)},
			"telemetry": {Name: dockerTelemetryVolume, Labels: runtimedocker.SystemLabels(runtimedocker.RoleTelemetryStorage)},
			"registry":  {Name: dockerRegistryVolume, Labels: runtimedocker.SystemLabels(runtimedocker.RoleRegistryStorage)},
		},
	}
	if !bundledRegistry {
		delete(compose.Volumes, "registry")
	}
	raw, err := yaml.Marshal(compose)
	if err != nil {
		return nil, features, err
	}
	files["compose.yaml"] = bundleFile{Data: raw}
	return files, features, nil
}

func addComposeProxyEnv(env map[string]string, proxy config.InstallProxy) {
	resolved := proxyEnvMap(proxy)
	for key, value := range resolved {
		switch key {
		case "HTTP_PROXY", "http_proxy":
			env[key] = "${HTTP_PROXY:-}"
		case "HTTPS_PROXY", "https_proxy":
			env[key] = "${HTTPS_PROXY:-}"
		default:
			env[key] = value
		}
	}
}

const composeSecretsExample = `# Copy to secrets.env, fill the values, and keep that file out of source control.
# up.sh sources this as shell syntax; quote values containing shell characters.
DONKEY_REGISTRY_PASSWORD=
DONKEY_PACKAGES_PASSWORD=
DONKEY_OIDC_CLIENT_SECRET=
OPENAI_API_KEY=
DONKEY_SESSION_KEY=
DONKEY_TELEMETRY_KEY=
DONKEY_BUILD_ENV=
DONKEY_BUILDER_INSTRUCTIONS=
HTTP_PROXY=
HTTPS_PROXY=

# Change both only if the default subnet conflicts with an existing network.
# DONKEY_NETWORK_SUBNET=172.31.250.0/24
# DONKEY_PACKAGES_IP=172.31.250.10
`

// composeIdentityFormat renders the docker-inspect Go template printing the
// immutable identity labels in the exact order the runtime's sameIdentity
// compares them, so the generated preflight cannot drift from the installer.
func composeIdentityFormat(path string) string {
	var parts []string
	for _, key := range runtimedocker.IdentityLabelKeys() {
		parts = append(parts, fmt.Sprintf("{{index . %q}}", key))
	}
	return fmt.Sprintf("{{with %s}}%s{{end}}", path, strings.Join(parts, "|"))
}

func renderComposeUpScript(settings *config.InstallFile, features composeSecretFeatures, bundledRegistry bool) string {
	clientID := settings.Auth.ClientID
	if clientID == "" {
		clientID = pendingOIDC
	}
	var b strings.Builder
	b.WriteString(`#!/bin/sh
set -eu

bundle_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -f "$bundle_dir/secrets.env" ]; then
  set -a
  . "$bundle_dir/secrets.env"
  set +a
fi

require_owned() {
  kind=$1
  name=$2
  role=$3
  case "$kind" in
    container)
      actual=$(docker container inspect "$name" --format '` + composeIdentityFormat(".Config.Labels") + `' 2>/dev/null) || return 0
      ;;
    network)
      actual=$(docker network inspect "$name" --format '` + composeIdentityFormat(".Labels") + `' 2>/dev/null) || return 0
      ;;
    volume)
      actual=$(docker volume inspect "$name" --format '` + composeIdentityFormat(".Labels") + `' 2>/dev/null) || return 0
      ;;
  esac
	  expected="true|donkey||||$role"
	  [ "$actual" = "$expected" ] || {
	    echo "the Docker $kind $name exists but is not this donkey installation's $role resource" >&2
    exit 1
  }
}

`)
	fmt.Fprintf(&b, "require_owned network %s %s\n", dockerNetwork, runtimedocker.RoleNetwork)
	for _, item := range dockerInstallVolumes(bundledRegistry) {
		fmt.Fprintf(&b, "require_owned volume %s %s\n", item.name, item.role)
	}
	for _, item := range dockerInstallContainers(bundledRegistry) {
		fmt.Fprintf(&b, "require_owned container %s %s\n", item.name, item.role)
	}
	b.WriteString(`
# Env entries are emitted with a unit-separator delimiter: a line-based read
# would truncate multi-line values (builder instructions) to their first line.
existing() {
  docker inspect "$1" --format '{{range .Config.Env}}{{.}}{{printf "\x1f"}}{{end}}' 2>/dev/null \
    | awk -v RS="$(printf '\037')" -v k="$2" 'index($0, k "=") == 1 { printf "%s", substr($0, length(k) + 2); exit }' || true
}

DONKEY_REGISTRY_PASSWORD="${DONKEY_REGISTRY_PASSWORD:-$(existing donkey-server DONKEY_REGISTRY_PASSWORD)}"
DONKEY_PACKAGES_PASSWORD="${DONKEY_PACKAGES_PASSWORD:-$(existing donkey-packages DONKEY_PACKAGES_PASSWORD)}"
DONKEY_OIDC_CLIENT_SECRET="${DONKEY_OIDC_CLIENT_SECRET:-$(existing donkey-server DONKEY_OIDC_CLIENT_SECRET)}"
OPENAI_API_KEY="${OPENAI_API_KEY:-$(existing donkey-server OPENAI_API_KEY)}"
DONKEY_SESSION_KEY="${DONKEY_SESSION_KEY:-$(existing donkey-server DONKEY_SESSION_KEY)}"
DONKEY_TELEMETRY_KEY="${DONKEY_TELEMETRY_KEY:-$(existing donkey-telemetry DONKEY_TELEMETRY_KEY)}"
DONKEY_BUILD_ENV="${DONKEY_BUILD_ENV:-$(existing donkey-server DONKEY_BUILD_ENV)}"
DONKEY_BUILDER_INSTRUCTIONS="${DONKEY_BUILDER_INSTRUCTIONS:-$(existing donkey-server DONKEY_BUILDER_INSTRUCTIONS)}"
HTTP_PROXY="${HTTP_PROXY:-$(existing donkey-server HTTP_PROXY)}"
HTTPS_PROXY="${HTTPS_PROXY:-$(existing donkey-server HTTPS_PROXY)}"
`)
	if clientID != pendingOIDC {
		b.WriteString("[ -n \"$DONKEY_OIDC_CLIENT_SECRET\" ] || { echo 'DONKEY_OIDC_CLIENT_SECRET is required' >&2; exit 1; }\n")
	}
	b.WriteString("[ -n \"$OPENAI_API_KEY\" ] || { echo 'OPENAI_API_KEY is required' >&2; exit 1; }\n")
	if features.RegistryCredentials {
		b.WriteString("[ -n \"$DONKEY_REGISTRY_PASSWORD\" ] || { echo 'DONKEY_REGISTRY_PASSWORD is required' >&2; exit 1; }\n")
	}
	if features.PackagePassword {
		b.WriteString("[ -n \"$DONKEY_PACKAGES_PASSWORD\" ] || { echo 'DONKEY_PACKAGES_PASSWORD is required' >&2; exit 1; }\n")
	}
	if features.BuildEnvironment {
		b.WriteString("[ -n \"$DONKEY_BUILD_ENV\" ] || { echo 'DONKEY_BUILD_ENV is required because build.env is secret-backed' >&2; exit 1; }\n")
	}
	if features.BuilderInstructions {
		b.WriteString("[ -n \"$DONKEY_BUILDER_INSTRUCTIONS\" ] || { echo 'DONKEY_BUILDER_INSTRUCTIONS is required because build.instructions is secret-backed' >&2; exit 1; }\n")
	}
	if features.HTTPForwardProxy {
		b.WriteString("[ -n \"$HTTP_PROXY\" ] || { echo 'HTTP_PROXY is required because build.proxy.http is secret-backed' >&2; exit 1; }\n")
	}
	if features.HTTPSForwardProxy {
		b.WriteString("[ -n \"$HTTPS_PROXY\" ] || { echo 'HTTPS_PROXY is required because build.proxy.https is secret-backed' >&2; exit 1; }\n")
	}
	b.WriteString(`if [ -z "$DONKEY_SESSION_KEY" ]; then DONKEY_SESSION_KEY=$(openssl rand -hex 32); fi
if [ -z "$DONKEY_TELEMETRY_KEY" ]; then DONKEY_TELEMETRY_KEY=$(openssl rand -hex 32); fi

export DONKEY_REGISTRY_PASSWORD DONKEY_PACKAGES_PASSWORD DONKEY_OIDC_CLIENT_SECRET
export OPENAI_API_KEY DONKEY_SESSION_KEY DONKEY_TELEMETRY_KEY
export DONKEY_BUILD_ENV DONKEY_BUILDER_INSTRUCTIONS HTTP_PROXY HTTPS_PROXY

docker compose -f "$bundle_dir/compose.yaml" up -d
`)
	serverHost := settings.ServerHost
	if serverHost == "" {
		serverHost = settings.Domain
	}
	fmt.Fprintf(&b, "echo %s\n", shellQuote("Donkey is starting at https://"+serverHost))
	return b.String()
}

func renderComposeReadme(settings *config.InstallFile, bundledRegistry bool) string {
	registry := "an external registry"
	if bundledRegistry {
		registry = "the bundled loopback registry"
	}
	serverHost := settings.ServerHost
	if serverHost == "" {
		serverHost = settings.Domain
	}
	return fmt.Sprintf(`# Donkey Docker Compose deployment

This bundle runs the same control-plane roles as server install,
using exact container and volume names so donkey's Docker runtime can attach
apps to them. App images use %s.

1. Copy secrets.env.example to secrets.env and fill the required values. Keep
   it out of source control. up.sh preserves values from existing containers
   and creates session and telemetry keys only when no prior value exists. It
   first refuses any same-named container, network or volume that does not
   carry Donkey's exact control-plane ownership labels.
2. Review compose.yaml and buildkitd.toml.
3. Run ./up.sh.

The server API is also bound to 127.0.0.1:8088; public traffic enters on ports
80 and 443. The package proxy has a stable address because BuildKit RUN steps
cannot use Docker's loopback DNS. If %s conflicts with another Docker network,
set DONKEY_NETWORK_SUBNET and DONKEY_PACKAGES_IP together before running up.sh.

For a private control-plane image, log this Docker engine into its registry
before starting Compose. DONKEY_BUILD_ENV is the JSON form of build.env;
builder instructions and credentialed proxies use DONKEY_BUILDER_INSTRUCTIONS,
HTTP_PROXY and HTTPS_PROXY. Leave those entries empty when the corresponding
settings are not configured. Open https://%s after the services are running.
`, registry, composeNetworkSubnet, serverHost)
}
