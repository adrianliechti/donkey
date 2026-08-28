package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// The Docker installation: donkey straight on a Docker engine, no cluster.
// The registry, build engine, package proxy, server, gateway and AI endpoint
// share one infrastructure network. Only the server gets the Docker socket;
// only the package proxy gets package credentials.
const (
	dockerNetwork    = "donkey"
	dockerDataVolume = "donkey-data"
	dockerDataDir    = "/var/lib/donkey"

	dockerRegistryName   = "donkey-registry"
	dockerRegistryImage  = "registry:3"
	dockerRegistryVolume = "donkey-registry"
	// The registry publishes on the host's loopback so the Docker daemon
	// can pull what the build engine pushed — localhost registries are
	// insecure-allowed by default, no daemon configuration needed.
	dockerRegistryHostPort = "5000"
	dockerRegistryCAPath   = "/tmp/donkey-registry-ca.crt"
	dockerRegistryAuthPath = "/etc/donkey/registry-auth"

	dockerBuildkitName   = "donkey-buildkitd"
	dockerBuildkitVolume = "donkey-buildkit"

	dockerServerName      = "donkey-server"
	dockerGatewayName     = "donkey-gateway"
	dockerAIName          = "donkey-ai"
	dockerTelemetryName   = "donkey-telemetry"
	dockerTelemetryVolume = "donkey-telemetry"
	dockerPackagesName    = "donkey-packages"
	dockerPackagesAuth    = "/etc/donkey/package-auth"
)

// dockerNoProxy is what must never be sent to a forward proxy: donkey's own
// containers, the Docker engine's socket and the loopback registry all live
// on this machine, and no corporate proxy can route them.
const dockerNoProxy = "localhost,127.0.0.1," +
	dockerRegistryName + "," + dockerBuildkitName + "," + dockerServerName + "," + dockerGatewayName + "," + dockerAIName + "," + dockerTelemetryName + "," + dockerPackagesName

// dockerInstallAction stands the whole control plane up on the local Docker
// engine (or DOCKER_HOST).
func dockerInstallAction(ctx context.Context, u *ui.UI, settings *config.InstallFile) error {
	// The local registry is part of the installation here — no external one
	// to point at (though an explicit registry.url still wins).
	bundledRegistry := settings.Registry.URL == ""
	if bundledRegistry {
		settings.Registry.URL = dockerRegistryName + ":5000/donkey"
		settings.Registry.Insecure = true
	}
	if missing := missingSettings(settings, false); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	if err := settings.Registry.Validate(); err != nil {
		return err
	}
	if err := settings.Registry.ValidateRuntime("docker"); err != nil {
		return err
	}
	switch settings.TLS.Mode {
	case "", "acme", "ca":
	default:
		return fmt.Errorf("tls.mode must be acme or ca on the docker runtime, not %q", settings.TLS.Mode)
	}
	if err := settings.Build.Validate(); err != nil {
		return err
	}
	if err := settings.Build.Packages.ValidateRuntime("docker"); err != nil {
		return err
	}
	if err := settings.AddonImages.Validate(); err != nil {
		return err
	}

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("the Docker engine didn't answer — is Docker running? (%w)", err)
	}

	// Keep the session key on re-installs — rotating it signs everyone out.
	sessionKey := existingContainerEnv(ctx, cli, dockerServerName, "DONKEY_SESSION_KEY")
	if sessionKey == "" {
		if sessionKey, err = randomHexKey(); err != nil {
			return err
		}
	}
	telemetryKey := existingContainerEnv(ctx, cli, dockerTelemetryName, "DONKEY_TELEMETRY_KEY")
	if telemetryKey == "" {
		telemetryKey = existingContainerEnv(ctx, cli, dockerServerName, "DONKEY_TELEMETRY_KEY")
	}
	if telemetryKey == "" {
		if telemetryKey, err = randomHexKey(); err != nil {
			return err
		}
	}
	if settings.AI.APIKey == "" {
		settings.AI.APIKey = existingContainerEnv(ctx, cli, dockerServerName, "OPENAI_API_KEY")
	}
	if settings.Auth.ClientSecret == "" {
		settings.Auth.ClientSecret = existingContainerEnv(ctx, cli, dockerServerName, "DONKEY_OIDC_CLIENT_SECRET")
	}
	if missing := missingSettings(settings, true); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	// Same as the cluster install: prove the assistant answers before the
	// installation changes.
	if err := checkAssistant(ctx, u, settings.AI); err != nil {
		return err
	}
	if settings.Auth.ClientID == "" {
		settings.Auth.ClientID = pendingOIDC
		settings.Auth.ClientSecret = pendingOIDC
	}
	buildkitImage := settings.BuildkitImage
	if buildkitImage == "" {
		buildkitImage = config.DefaultBuildkitImage
	}
	registryUsername, registryPassword, err := settings.Registry.Credentials()
	if err != nil {
		return err
	}
	buildkitPullAuth, err := registryPullAuth(settings.Registry.Registry, registryUsername, registryPassword, buildkitImage)
	if err != nil {
		return err
	}
	serverPullAuth, err := registryPullAuth(settings.Registry.Registry, registryUsername, registryPassword, settings.Image)
	if err != nil {
		return err
	}
	if settings.Build.Packages.Password == "" && settings.Build.Packages.NeedsPassword() {
		settings.Build.Packages.Password = existingContainerEnv(ctx, cli, dockerPackagesName, "DONKEY_PACKAGES_PASSWORD")
	}
	if err := settings.Build.Packages.ValidateCredentials(); err != nil {
		return err
	}

	u.Printf("Installing the donkey control plane onto the Docker engine…")

	if err := ensureDockerNetwork(ctx, cli); err != nil {
		return err
	}
	if bundledRegistry {
		// The registry: no host port beyond the loopback, no credentials — it
		// only exists on donkey's networks and the host's own daemon.
		if err := replaceInfraContainer(ctx, cli, infraContainer{
			Name:  dockerRegistryName,
			Image: dockerRegistryImage,
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: dockerRegistryVolume, Target: "/var/lib/registry"},
			},
			Ports: map[string]string{"127.0.0.1:" + dockerRegistryHostPort: "5000"},
		}); err != nil {
			return err
		}
	}

	// The build engine gets the push registry's transport settings and the
	// mirrors used when this installation cannot reach public registries.
	buildkitConfig := manifests.BuildkitConfig{
		RegistryHost:      settings.Registry.Host(),
		RegistryPlainHTTP: settings.Registry.Insecure,
		Mirrors:           settings.Build.RegistryMirrors,
	}
	buildkitFiles := map[string][]byte{}
	serverFiles := map[string][]byte{}
	packagesFiles := map[string][]byte{}
	if settings.Registry.CAFile != "" {
		ca, err := os.ReadFile(settings.Registry.CAFile)
		if err != nil {
			return fmt.Errorf("reading the registry's certificate authority: %w", err)
		}
		buildkitConfig.RegistryCAFile = dockerRegistryCAPath
		buildkitFiles[dockerRegistryCAPath] = ca
		serverFiles[dockerRegistryCAPath] = ca
		packagesFiles[dockerRegistryCAPath] = ca
	}
	buildkitFiles["/etc/buildkit/buildkitd.toml"] = []byte(manifests.BuildkitTOML(buildkitConfig))

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
		"DONKEY_PACKAGES_PASSWORD":         settings.Build.Packages.Password,
	}
	if settings.Registry.CAFile != "" {
		packagesEnv["DONKEY_PACKAGES_CA_FILE"] = dockerRegistryCAPath
	}
	packagesMounts := []mount.Mount{}
	if settings.Build.Packages.DockerConfigDir != "" {
		packagesEnv["DONKEY_PACKAGES_DOCKER_CONFIG_DIR"] = dockerPackagesAuth
		packagesEnv["DONKEY_PACKAGES_DOCKER_CONFIG_HOST"] = settings.Build.Packages.DockerConfigHost
		packagesMounts = append(packagesMounts, mount.Mount{
			Type: mount.TypeBind, Source: settings.Build.Packages.DockerConfigDir,
			Target: dockerPackagesAuth, ReadOnly: true,
		})
	}
	maps.Copy(packagesEnv, proxyEnvMap(settings.Build.Proxy))
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerPackagesName,
		Image:        settings.Image,
		RegistryAuth: serverPullAuth,
		Cmd:          []string{"packages"},
		Env:          packagesEnv,
		Mounts:       packagesMounts,
		Files:        packagesFiles,
		Healthcheck: &container.HealthConfig{
			Test:     []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8090/readyz"},
			Interval: 2 * time.Second, Timeout: 6 * time.Second, Retries: 3,
		},
		PullAlways: true,
	}); err != nil {
		return err
	}

	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerBuildkitName,
		Image:        buildkitImage,
		RegistryAuth: buildkitPullAuth,
		Cmd:          []string{"--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml"},
		Privileged:   true,
		Env:          proxyEnvMap(settings.Build.Proxy),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: dockerBuildkitVolume, Target: "/var/lib/buildkit"},
		},
		Files: buildkitFiles,
	}); err != nil {
		return err
	}

	serverHost := settings.ServerHost
	if serverHost == "" {
		serverHost = settings.Domain
	}
	tlsMode := settings.TLS.Mode
	if tlsMode == "" {
		tlsMode = "acme"
	}

	serverEnv := map[string]string{
		"DONKEY_RUNTIME":            "docker",
		"DONKEY_STATE_DIR":          dockerDataDir,
		"DONKEY_LISTEN":             ":8088",
		"DONKEY_DOMAIN":             settings.Domain,
		"DONKEY_SERVER_HOST":        serverHost,
		"DONKEY_REGISTRY_URL":       settings.Registry.URL,
		"DONKEY_REGISTRY_USERNAME":  registryUsername,
		"DONKEY_REGISTRY_INSECURE":  strconv.FormatBool(settings.Registry.Insecure),
		"DONKEY_REGISTRY_IMMUTABLE": strconv.FormatBool(settings.Registry.Immutable),
		"DONKEY_BUILDKIT_ADDR":      "tcp://" + dockerBuildkitName + ":1234",
		"DONKEY_PACKAGES_URL":       "http://" + dockerPackagesName + ":8090",
		"DONKEY_AI_URL":             "http://" + dockerAIName + ":8089",
		"DONKEY_TELEMETRY_URL":      "http://" + dockerTelemetryName + ":9090",
		"DONKEY_DEFAULT_PLATFORM":   settings.DefaultPlatform,
		"DONKEY_POSTGRES_IMAGE":     settings.AddonImages.Postgres,
		"DONKEY_RUSTFS_IMAGE":       settings.AddonImages.RustFS,
		"DONKEY_OIDC_ISSUER":        settings.Auth.Issuer,
		"DONKEY_OIDC_CLIENT_ID":     settings.Auth.ClientID,
		"DONKEY_OIDC_CLIENT_SECRET": settings.Auth.ClientSecret,
		"DONKEY_SESSION_KEY":        sessionKey,
		"DONKEY_TELEMETRY_KEY":      telemetryKey,
		"DONKEY_ADMINS":             adminsJSONString(settings.Auth.Admins),
		"DONKEY_AI_MODEL":           settings.AI.Model,
		"OPENAI_BASE_URL":           settings.AI.BaseURL,
		"OPENAI_API_KEY":            settings.AI.APIKey,
	}
	if bundledRegistry {
		serverEnv["DONKEY_PULL_REGISTRY"] = "127.0.0.1:" + dockerRegistryHostPort
	}
	if settings.Registry.CAFile != "" {
		serverEnv["DONKEY_REGISTRY_CA_FILE"] = dockerRegistryCAPath
	}
	serverMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		{Type: mount.TypeVolume, Source: dockerDataVolume, Target: dockerDataDir},
	}
	if settings.Registry.DockerConfigDir != "" {
		serverEnv["DONKEY_REGISTRY_CONFIG_DIR"] = dockerRegistryAuthPath
		serverMounts = append(serverMounts, mount.Mount{
			Type: mount.TypeBind, Source: settings.Registry.DockerConfigDir,
			Target: dockerRegistryAuthPath, ReadOnly: true,
		})
	}
	maps.Copy(serverEnv, builderEnvMap(settings.Build.BuildSettings))
	if len(settings.Build.Env) > 0 {
		raw, err := json.Marshal(settings.Build.Env)
		if err != nil {
			return err
		}
		serverEnv["DONKEY_BUILD_ENV"] = string(raw)
	}
	// The server resolves versions and image configuration itself, so it
	// needs the proxy as much as the build engine does.
	maps.Copy(serverEnv, proxyEnvMap(settings.Build.Proxy))
	// A password only exists for an external registry — the bundled one is
	// loopback-only and credential-less.
	if registryPassword != "" {
		serverEnv["DONKEY_REGISTRY_PASSWORD"] = registryPassword
	}
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerTelemetryName,
		Image:        settings.Image,
		RegistryAuth: serverPullAuth,
		Cmd:          []string{"telemetry"},
		Env: map[string]string{
			"DONKEY_TELEMETRY_LISTEN":     ":4318",
			"DONKEY_TELEMETRY_API_LISTEN": ":9090",
			"DONKEY_TELEMETRY_DB":         "/var/lib/donkey/telemetry.duckdb",
			"DONKEY_TELEMETRY_KEY":        telemetryKey,
		},
		Mounts: []mount.Mount{{
			Type: mount.TypeVolume, Source: dockerTelemetryVolume, Target: "/var/lib/donkey",
		}},
		Healthcheck: &container.HealthConfig{
			Test:     []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9090/readyz"},
			Interval: 2 * time.Second, Timeout: 6 * time.Second, Retries: 3,
		},
		PullAlways: true,
	}); err != nil {
		return err
	}
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerServerName,
		Image:        settings.Image,
		RegistryAuth: serverPullAuth,
		Env:          serverEnv,
		Mounts:       serverMounts,
		// The API on the host's loopback is the direct, token-verified path
		// past the gateway — what `kubectl port-forward` is on Kubernetes.
		Ports:      map[string]string{"127.0.0.1:8088": "8088"},
		Files:      serverFiles,
		PullAlways: true,
	}); err != nil {
		return err
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
		"DONKEY_OIDC_CLIENT_ID":          settings.Auth.ClientID,
		"DONKEY_OIDC_CLIENT_SECRET":      settings.Auth.ClientSecret,
		"DONKEY_SESSION_KEY":             sessionKey,
		"DONKEY_TELEMETRY_KEY":           telemetryKey,
		"DONKEY_TELEMETRY_OTLP_ENDPOINT": "http://" + dockerTelemetryName + ":4318",
	}
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerGatewayName,
		Image:        settings.Image,
		RegistryAuth: serverPullAuth,
		Cmd:          []string{"gateway"},
		Env:          gatewayEnv,
		Mounts: []mount.Mount{
			// The state volume is shared: the server writes routes and env,
			// the gateway reads routes and keeps its certificates there.
			{Type: mount.TypeVolume, Source: dockerDataVolume, Target: dockerDataDir},
		},
		Ports:      map[string]string{"80": "8090", "443": "8443"},
		PullAlways: true,
	}); err != nil {
		return err
	}

	// The openai addon's endpoint, apart from the server: it is the one
	// container apps are meant to reach, and it holds only the upstream
	// credential — no session key, no registry login, no docker socket.
	aiEnv := map[string]string{
		"DONKEY_RUNTIME":   "docker",
		"DONKEY_STATE_DIR": dockerDataDir,
		"DONKEY_LISTEN":    ":8089",
		"OPENAI_BASE_URL":  settings.AI.BaseURL,
		"OPENAI_API_KEY":   settings.AI.APIKey,
	}
	// The model API is the one thing this container talks to, and it is
	// outside — so of all of them, this is the one that needs the proxy.
	maps.Copy(aiEnv, proxyEnvMap(settings.Build.Proxy))
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:         dockerAIName,
		Image:        settings.Image,
		RegistryAuth: serverPullAuth,
		Cmd:          []string{"ai"},
		Env:          aiEnv,
		Mounts: []mount.Mount{
			// Read-only: the bindings it validates tokens against are the
			// server's records, and it never writes any of them.
			{Type: mount.TypeVolume, Source: dockerDataVolume, Target: dockerDataDir, ReadOnly: true},
		},
		PullAlways: true,
	}); err != nil {
		return err
	}

	u.Info("Waiting for the server to come up…")
	if err := waitDockerHealthy(ctx, cli, dockerPackagesName, 90*time.Second); err != nil {
		return err
	}
	if err := waitDockerHealthy(ctx, cli, dockerTelemetryName, 90*time.Second); err != nil {
		return err
	}
	if err := waitDockerRunning(ctx, cli, dockerServerName, 90*time.Second); err != nil {
		return err
	}
	if err := waitDockerRunning(ctx, cli, dockerGatewayName, 90*time.Second); err != nil {
		return err
	}
	if err := waitDockerRunning(ctx, cli, dockerAIName, 90*time.Second); err != nil {
		return err
	}
	// Both containers were just recreated, which dropped the space networks
	// they were on. Re-attach them, or every app deployed before this install
	// stays unreachable until it is redeployed.
	if err := docker.AttachOwnContainers(ctx, cli, dockerServerName, dockerGatewayName, dockerAIName, dockerTelemetryName); err != nil {
		return err
	}
	u.Success("Control plane is running.")

	serverURL := "https://" + serverHost
	u.Printf("")
	u.Printf("  API:        %s", u.Bold(serverURL))
	u.Printf("  Builder:    %s", u.Faint(builderSummary(settings.Build)))
	u.Printf("")
	u.Printf("Console:  open %s and sign in", u.Bold(serverURL))
	u.Printf("CLI:      %s  %s", u.Bold(fmt.Sprintf("donkey login --server %s", serverURL)), u.Faint("(opens the browser)"))
	u.Printf("Then check the whole setup end to end:  %s", u.Bold("donkey doctor"))
	u.Printf("")
	u.Info("Apps are served at https://<app>.<space>.%s.", settings.Domain)
	u.Printf("  %s", u.Faint(fmt.Sprintf("Point *.%s and %s itself at this machine — the gateway answers on ports 80 and 443.", settings.Domain, settings.Domain)))
	switch tlsMode {
	case "acme":
		u.Printf("  %s", u.Faint("TLS: the gateway requests one certificate per hostname via ACME (Let's Encrypt) — this machine must be reachable from the internet on ports 80 and 443."))
	case "ca":
		u.Printf("  %s", u.Faint(fmt.Sprintf(
			"TLS: the gateway signs certificates with its own CA. Export it once and trust it: docker run --rm -v %s:/state busybox cat /state/ca/ca.crt", dockerDataVolume)))
	}
	if settings.Auth.ClientID == pendingOIDC {
		u.Warn("No OIDC client configured yet — nobody can sign in until you register one at the issuer and re-run this install with the client id and secret.")
	}
	return nil
}

// builderEnvMap is the builder's settings; only what was chosen travels, so
// the server keeps its defaults for everything else.
func builderEnvMap(b config.BuildSettings) map[string]string {
	env := map[string]string{}
	if b.Instructions != "" {
		env["DONKEY_BUILDER_INSTRUCTIONS"] = b.Instructions
	}
	if b.Verify != nil {
		env["DONKEY_BUILDER_VERIFY"] = strconv.FormatBool(*b.Verify)
	}
	if b.VerifySeconds > 0 {
		env["DONKEY_BUILDER_VERIFY_SECONDS"] = strconv.Itoa(b.VerifySeconds)
	}
	if b.Attempts > 0 {
		env["DONKEY_BUILDER_ATTEMPTS"] = strconv.Itoa(b.Attempts)
	}
	return env
}

// proxyEnvMap is the forward proxy in the spelling every tool already reads.
// Both cases: some tools read only the lowercase form, and Go's own HTTP
// client ignores an uppercase HTTP_PROXY for CGI-safety reasons.
func proxyEnvMap(p config.InstallProxy) map[string]string {
	env := map[string]string{}
	if p.HTTP != "" || p.HTTPS != "" {
		// donkey's containers reach each other by name on their own Docker
		// network; a corporate proxy cannot route those.
		p.NoProxy = strings.Trim(strings.TrimSpace(p.NoProxy)+","+dockerNoProxy, ",")
	}
	for name, value := range map[string]string{
		"HTTP_PROXY":  p.HTTP,
		"HTTPS_PROXY": p.HTTPS,
		"NO_PROXY":    p.NoProxy,
	} {
		if value == "" {
			continue
		}
		env[name] = value
		env[strings.ToLower(name)] = value
	}
	return env
}

func adminsJSONString(admins *config.AccessConfig) string {
	if admins.Empty() {
		return ""
	}
	b, err := json.Marshal(admins)
	if err != nil {
		return ""
	}
	return string(b)
}

func ensureDockerNetwork(ctx context.Context, cli *dockerclient.Client) error {
	_, err := cli.NetworkInspect(ctx, dockerNetwork, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		_, err = cli.NetworkCreate(ctx, dockerNetwork, network.CreateOptions{})
	}
	return err
}

type infraContainer struct {
	Name         string
	Image        string
	RegistryAuth string
	Cmd          []string
	Env          map[string]string
	Mounts       []mount.Mount
	Ports        map[string]string // "host" or "ip:port" -> container port
	Files        map[string][]byte // written into the container before start
	Healthcheck  *container.HealthConfig
	Privileged   bool
	PullAlways   bool
}

// replaceInfraContainer recreates one of donkey's own containers — the
// Docker spelling of an idempotent install: same name, fresh config.
func replaceInfraContainer(ctx context.Context, cli *dockerclient.Client, spec infraContainer) error {
	if err := pullInfraImage(ctx, cli, spec.Image, spec.RegistryAuth, spec.PullAlways); err != nil {
		return err
	}
	if err := cli.ContainerRemove(ctx, spec.Name, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for host, ctr := range spec.Ports {
		port := nat.Port(ctr + "/tcp")
		exposed[port] = struct{}{}
		ip, hostPort := "", host
		if strings.Contains(host, ":") {
			ip, hostPort, _ = strings.Cut(host, ":")
		}
		bindings[port] = append(bindings[port], nat.PortBinding{HostIP: ip, HostPort: hostPort})
	}
	cfg := &container.Config{
		Image:        spec.Image,
		Cmd:          spec.Cmd,
		Env:          env,
		ExposedPorts: exposed,
		Healthcheck:  spec.Healthcheck,
	}
	hostCfg := &container.HostConfig{
		Mounts:        spec.Mounts,
		Privileged:    spec.Privileged,
		PortBindings:  bindings,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{dockerNetwork: {}},
	}
	if _, err := cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name); err != nil {
		return err
	}
	for path, data := range spec.Files {
		if err := copyFileIntoContainer(ctx, cli, spec.Name, path, data); err != nil {
			return err
		}
	}
	return cli.ContainerStart(ctx, spec.Name, container.StartOptions{})
}

func pullInfraImage(ctx context.Context, cli *dockerclient.Client, ref, registryAuth string, always bool) error {
	if !always {
		if _, err := cli.ImageInspect(ctx, ref); err == nil {
			return nil
		}
	}
	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		// The image may only exist locally (a dev build never pushed).
		if _, ierr := cli.ImageInspect(ctx, ref); ierr == nil {
			return nil
		}
		return fmt.Errorf("couldn't pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// registryPullAuth authenticates Docker-daemon pulls from the configured
// enterprise registry. The URL may include a repository prefix; Docker auth
// is intentionally keyed to the host only.
func registryPullAuth(reg config.Registry, username, password, ref string) (string, error) {
	host := reg.Host()
	if host == "" || username == "" || password == "" || !strings.HasPrefix(ref, host+"/") {
		return "", nil
	}
	raw, err := json.Marshal(map[string]string{
		"username":      username,
		"password":      password,
		"serveraddress": host,
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

// copyFileIntoContainer writes one file into a created (not yet started)
// container — how the build engine gets its config without a host path.
func copyFileIntoContainer(ctx context.Context, cli *dockerclient.Client, name, path string, data []byte) error {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name: strings.TrimPrefix(path, "/"),
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return cli.CopyToContainer(ctx, name, "/", &tarBuf, container.CopyToContainerOptions{})
}

// existingContainerEnv reads one env var off an existing container — how a
// re-install keeps the secrets the running installation depends on.
func existingContainerEnv(ctx context.Context, cli *dockerclient.Client, name, key string) string {
	info, err := cli.ContainerInspect(ctx, name)
	if err != nil || info.Config == nil {
		return ""
	}
	for _, kv := range info.Config.Env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func waitDockerRunning(ctx context.Context, cli *dockerclient.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := cli.ContainerInspect(ctx, name)
		if err == nil && info.State != nil && info.State.Running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't come up — check its logs: docker logs %s", name, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func waitDockerHealthy(ctx context.Context, cli *dockerclient.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := cli.ContainerInspect(ctx, name)
		if err == nil && info.State != nil && info.State.Health != nil {
			switch info.State.Health.Status {
			case "healthy":
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't become ready — check its logs: docker logs %s", name, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
