package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// The Docker installation: donkey straight on a Docker engine, no cluster.
// Four containers on one network — the registry (builds land there), the
// build engine, donkey-server (with the Docker socket, it is the runtime
// provider) and donkey-gateway (ports 80/443, TLS and login) — plus one
// state volume the server writes and the gateway reads.
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

	dockerBuildkitName   = "donkey-buildkitd"
	dockerBuildkitVolume = "donkey-buildkit"

	dockerServerName  = "donkey-server"
	dockerGatewayName = "donkey-gateway"
)

// dockerNoProxy is what must never be sent to a forward proxy: donkey's own
// containers, the Docker engine's socket and the loopback registry all live
// on this machine, and no corporate proxy can route them.
const dockerNoProxy = "localhost,127.0.0.1," +
	dockerRegistryName + "," + dockerBuildkitName + "," + dockerServerName + "," + dockerGatewayName

// dockerInstallAction stands the whole control plane up on the local Docker
// engine (or DOCKER_HOST).
func dockerInstallAction(ctx context.Context, u *ui.UI, settings *config.InstallFile) error {
	// The local registry is part of the installation here — no external one
	// to point at (though an explicit registry.url still wins).
	if settings.Registry.URL == "" {
		settings.Registry.URL = dockerRegistryName + ":5000/donkey"
		settings.Registry.Insecure = true
	}
	if missing := missingSettings(settings, false); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	switch settings.TLS.Mode {
	case "", "acme", "ca", "static":
	default:
		return fmt.Errorf("tls.mode must be acme, ca or static, not %q", settings.TLS.Mode)
	}
	if err := settings.Build.Validate(); err != nil {
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
	if settings.AI.APIKey == "" {
		settings.AI.APIKey = existingContainerEnv(ctx, cli, dockerServerName, "OPENAI_API_KEY")
	}
	if settings.Auth.ClientSecret == "" {
		settings.Auth.ClientSecret = existingContainerEnv(ctx, cli, dockerServerName, "DONKEY_OIDC_CLIENT_SECRET")
	}
	if missing := missingSettings(settings, true); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	if settings.Auth.ClientID == "" {
		settings.Auth.ClientID = pendingOIDC
		settings.Auth.ClientSecret = pendingOIDC
	}
	buildkitImage := settings.BuildkitImage
	if buildkitImage == "" {
		buildkitImage = config.DefaultBuildkitImage
	}

	u.Printf("Installing the donkey control plane onto the Docker engine…")

	if err := ensureDockerNetwork(ctx, cli); err != nil {
		return err
	}
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

	// The build engine, told that the bundled registry speaks plain http —
	// and about the mirrors, if this installation only reaches registries
	// through one.
	buildkitToml := manifests.BuildkitTOML(manifests.BuildkitConfig{
		RegistryHost:      dockerRegistryName + ":5000",
		RegistryPlainHTTP: true,
		Mirrors:           settings.Build.RegistryMirrors,
	})
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:       dockerBuildkitName,
		Image:      buildkitImage,
		Cmd:        []string{"--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml"},
		Privileged: true,
		Env:        proxyEnvMap(settings.Build.Proxy),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: dockerBuildkitVolume, Target: "/var/lib/buildkit"},
		},
		Files: map[string][]byte{"/etc/buildkit/buildkitd.toml": []byte(buildkitToml)},
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
	tlsSecret := settings.TLS.Secret
	if tlsSecret == "" {
		tlsSecret = "donkey-gateway-tls"
	}

	serverEnv := map[string]string{
		"DONKEY_RUNTIME":            "docker",
		"DONKEY_STATE_DIR":          dockerDataDir,
		"DONKEY_LISTEN":             ":8088",
		"DONKEY_DOMAIN":             settings.Domain,
		"DONKEY_SERVER_HOST":        serverHost,
		"DONKEY_REGISTRY_URL":       settings.Registry.URL,
		"DONKEY_REGISTRY_USERNAME":  settings.Registry.Username,
		"DONKEY_REGISTRY_INSECURE":  strconv.FormatBool(settings.Registry.Insecure),
		"DONKEY_REGISTRY_IMMUTABLE": strconv.FormatBool(settings.Registry.Immutable),
		"DONKEY_PULL_REGISTRY":      "127.0.0.1:" + dockerRegistryHostPort,
		"DONKEY_BUILDKIT_ADDR":      "tcp://" + dockerBuildkitName + ":1234",
		"DONKEY_DEFAULT_PLATFORM":   settings.DefaultPlatform,
		"DONKEY_OIDC_ISSUER":        settings.Auth.Issuer,
		"DONKEY_OIDC_CLIENT_ID":     settings.Auth.ClientID,
		"DONKEY_OIDC_CLIENT_SECRET": settings.Auth.ClientSecret,
		"DONKEY_SESSION_KEY":        sessionKey,
		"DONKEY_ADMINS":             adminsJSONString(settings.Auth.Admins),
		"DONKEY_AI_MODEL":           settings.AI.Model,
		"OPENAI_BASE_URL":           settings.AI.BaseURL,
		"OPENAI_API_KEY":            settings.AI.APIKey,

		"DONKEY_BUILD_BUILDER":                string(settings.Build.Builder),
		"DONKEY_BUILD_RAILPACK_BUILDER_IMAGE": settings.Build.Railpack.BuilderImage,
		"DONKEY_BUILD_RAILPACK_RUNTIME_IMAGE": settings.Build.Railpack.RuntimeImage,
	}
	maps.Copy(serverEnv, aiBuilderEnvMap(settings.Build.AI))
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
	if pw, err := settings.Registry.Password(); err != nil {
		return err
	} else if pw != "" {
		serverEnv["DONKEY_REGISTRY_PASSWORD"] = pw
	}
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:  dockerServerName,
		Image: settings.Image,
		Env:   serverEnv,
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
			{Type: mount.TypeVolume, Source: dockerDataVolume, Target: dockerDataDir},
		},
		// The API on the host's loopback is the direct, token-verified path
		// past the gateway — what `kubectl port-forward` is on Kubernetes.
		Ports:      map[string]string{"127.0.0.1:8088": "8088"},
		PullAlways: true,
	}); err != nil {
		return err
	}

	gatewayEnv := map[string]string{
		"DONKEY_RUNTIME":            "docker",
		"DONKEY_STATE_DIR":          dockerDataDir,
		"DONKEY_LISTEN":             ":8090",
		"DONKEY_TLS_LISTEN":         ":8443",
		"DONKEY_HEALTH_LISTEN":      ":8091",
		"DONKEY_DOMAIN":             settings.Domain,
		"DONKEY_SERVER_HOST":        serverHost,
		"DONKEY_TLS_MODE":           tlsMode,
		"DONKEY_ACME_EMAIL":         settings.TLS.Email,
		"DONKEY_ACME_DIRECTORY":     settings.TLS.Directory,
		"DONKEY_TLS_SECRET":         tlsSecret,
		"DONKEY_OIDC_ISSUER":        settings.Auth.Issuer,
		"DONKEY_OIDC_CLIENT_ID":     settings.Auth.ClientID,
		"DONKEY_OIDC_CLIENT_SECRET": settings.Auth.ClientSecret,
		"DONKEY_SESSION_KEY":        sessionKey,
	}
	if err := replaceInfraContainer(ctx, cli, infraContainer{
		Name:  dockerGatewayName,
		Image: settings.Image,
		Cmd:   []string{"gateway"},
		Env:   gatewayEnv,
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

	u.Info("Waiting for the server to come up…")
	if err := waitDockerRunning(ctx, cli, dockerServerName, 90*time.Second); err != nil {
		return err
	}
	if err := waitDockerRunning(ctx, cli, dockerGatewayName, 90*time.Second); err != nil {
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
	case "static":
		u.Printf("  %s", u.Faint(fmt.Sprintf("TLS: served from tls.crt/tls.key under tls/ in the %s volume.", dockerDataVolume)))
	}
	if settings.Auth.ClientID == pendingOIDC {
		u.Warn("No OIDC client configured yet — nobody can sign in until you register one at the issuer and re-run this install with the client id and secret.")
	}
	return nil
}

// aiBuilderEnvMap is donkey's own builder's settings; only what was chosen
// travels, so the server keeps its defaults for everything else.
func aiBuilderEnvMap(ai config.InstallAIBuilder) map[string]string {
	env := map[string]string{}
	if ai.Verify != nil {
		env["DONKEY_BUILD_AI_VERIFY"] = strconv.FormatBool(*ai.Verify)
	}
	if ai.VerifySeconds > 0 {
		env["DONKEY_BUILD_AI_VERIFY_SECONDS"] = strconv.Itoa(ai.VerifySeconds)
	}
	if ai.Attempts > 0 {
		env["DONKEY_BUILD_AI_ATTEMPTS"] = strconv.Itoa(ai.Attempts)
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
	Name       string
	Image      string
	Cmd        []string
	Env        map[string]string
	Mounts     []mount.Mount
	Ports      map[string]string // "host" or "ip:port" -> container port
	Files      map[string][]byte // written into the container before start
	Privileged bool
	PullAlways bool
}

// replaceInfraContainer recreates one of donkey's own containers — the
// Docker spelling of an idempotent install: same name, fresh config.
func replaceInfraContainer(ctx context.Context, cli *dockerclient.Client, spec infraContainer) error {
	if err := pullInfraImage(ctx, cli, spec.Image, spec.PullAlways); err != nil {
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

func pullInfraImage(ctx context.Context, cli *dockerclient.Client, ref string, always bool) error {
	if !always {
		if _, err := cli.ImageInspect(ctx, ref); err == nil {
			return nil
		}
	}
	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{})
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
