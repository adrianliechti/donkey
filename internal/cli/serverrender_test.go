package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
	runtimedocker "github.com/adrianliechti/donkey/pkg/runtime/docker"
)

func TestServerInitPrintsConciseRuntimeTemplates(t *testing.T) {
	for _, runtimeName := range []string{"kubernetes", "docker"} {
		root := Command(Options{})
		var out bytes.Buffer
		root.Writer = &out
		if err := root.Run(context.Background(), []string{"donkey", "server", "init", "--runtime", runtimeName}); err != nil {
			t.Fatal(err)
		}
		want, err := serverInitTemplate(runtimeName)
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("%s template differs from command output", runtimeName)
		}
		var parsed config.InstallFile
		if err := yaml.UnmarshalStrict([]byte(want), &parsed); err != nil {
			t.Fatalf("%s template is not a strict install file: %v", runtimeName, err)
		}
		if parsed.Runtime != runtimeName {
			t.Fatalf("template runtime = %q, want %q", parsed.Runtime, runtimeName)
		}
	}
}

func TestExampleInstallFileIsGeneratedTemplate(t *testing.T) {
	got, err := os.ReadFile("../../examples/server-install.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != kubernetesInstallTemplate {
		t.Fatal("examples/server-install.yaml drifted from `donkey server init --runtime kubernetes`")
	}
}

func TestRuntimeImageOverridesReachBothDeploymentBundles(t *testing.T) {
	settings := &config.InstallFile{
		Image:           "registry.example.com/donkey-server:test",
		BusyboxImage:    "jfrog.corp/dockerhub/library/busybox:1.37",
		AddonImages:     config.InstallAddonImages{Redis: "jfrog.corp/dockerhub/library/redis"},
		Build:           config.InstallBuild{Packages: config.InstallPackages{Debian: "https://debian.mirror.example/debian", Alpine: "https://alpine.mirror.example/alpinelinux"}},
		CommonOfferings: config.CommonOfferings{"postgres", "redis"},
		Domain:          "apps.example.com",
		Registry:        config.InstallRegistry{Registry: config.Registry{URL: "registry.example.com/donkey"}},
		Auth:            config.InstallAuth{Issuer: "https://id.example.com"},
	}
	files, err := renderKubernetesBundle(settings)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(files["manifests.yaml"].Data); !strings.Contains(text, "name: DONKEY_BUSYBOX_IMAGE\n") || !strings.Contains(text, "value: "+settings.BusyboxImage) {
		t.Fatal("Kubernetes bundle did not pass busyboxImage to donkey-server")
	}
	if text := string(files["manifests.yaml"].Data); !strings.Contains(text, "name: DONKEY_REDIS_IMAGE\n") || !strings.Contains(text, "value: "+settings.AddonImages.Redis) {
		t.Fatal("Kubernetes bundle did not pass addonImages.redis to donkey-server")
	}
	if text := string(files["manifests.yaml"].Data); !strings.Contains(text, "name: DONKEY_COMMON_OFFERINGS\n") || !strings.Contains(text, settings.CommonOfferings.JSON()) {
		t.Fatal("Kubernetes bundle omitted common offerings")
	}
	files, err = renderDockerBundle(settings)
	if err != nil {
		t.Fatal(err)
	}
	var compose composeFile
	if err := yaml.Unmarshal(files["compose.yaml"].Data, &compose); err != nil {
		t.Fatal(err)
	}
	if got := compose.Services["server"].Environment["DONKEY_BUSYBOX_IMAGE"]; got != settings.BusyboxImage {
		t.Fatalf("Compose busyboxImage = %q", got)
	}
	if got := compose.Services["server"].Environment["DONKEY_REDIS_IMAGE"]; got != settings.AddonImages.Redis {
		t.Fatalf("Compose Redis image repository = %q", got)
	}
	if got := compose.Services["server"].Environment["DONKEY_COMMON_OFFERINGS"]; got != settings.CommonOfferings.JSON() {
		t.Fatal("Compose bundle omitted common offerings")
	}
	for key, want := range map[string]string{
		"DONKEY_PACKAGES_DEBIAN_URL": settings.Build.Packages.Debian,
		"DONKEY_PACKAGES_ALPINE_URL": settings.Build.Packages.Alpine,
	} {
		if got := compose.Services["packages"].Environment[key]; got != want {
			t.Errorf("Compose %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"DONKEY_BUILDER_DEBIAN_MIRROR", "DONKEY_BUILDER_ALPINE_MIRROR"} {
		if got := compose.Services["server"].Environment[key]; got != "true" {
			t.Errorf("Compose %s = %q, want true", key, got)
		}
	}
	// Direct Kubernetes install uses the same input as the bundle renderer.
	input := kubernetesSystemInput(settings, false, "", "", "session", "telemetry")
	if input.BusyboxImage != settings.BusyboxImage {
		t.Fatal("direct install omitted busyboxImage")
	}
	if input.CommonOfferings.JSON() != settings.CommonOfferings.JSON() {
		t.Fatal("direct Kubernetes install omitted common offerings")
	}
	settings.BusyboxImage = "https://jfrog.corp/busybox:1.37"
	if err := validateKubernetesSettings(settings); err == nil {
		t.Fatal("Kubernetes install accepted a URL instead of an image reference")
	}
	if err := validateDockerSettings(settings); err == nil {
		t.Fatal("Docker install accepted a URL instead of an image reference")
	}
}

func TestRegistryImageUsedOnlyForBundledDockerRegistry(t *testing.T) {
	settings := &config.InstallFile{
		Image:  "registry.example.com/donkey-server:test",
		Domain: "apps.example.com",
		Auth:   config.InstallAuth{Issuer: "https://id.example.com"},
	}
	for _, image := range []string{"", "jfrog.corp/dockerhub/library/registry:3"} {
		settings.RegistryImage = image
		files, err := renderDockerBundle(settings)
		if err != nil {
			t.Fatal(err)
		}
		var compose composeFile
		if err := yaml.Unmarshal(files["compose.yaml"].Data, &compose); err != nil {
			t.Fatal(err)
		}
		want := image
		if want == "" {
			want = "registry:3"
		}
		if got := compose.Services["registry"].Image; got != want {
			t.Fatalf("bundled registry image = %q, want %q", got, want)
		}
	}
	settings.Registry.URL = "external.example.com/donkey"
	files, err := renderDockerBundle(settings)
	if err != nil {
		t.Fatal(err)
	}
	var compose composeFile
	if err := yaml.Unmarshal(files["compose.yaml"].Data, &compose); err != nil {
		t.Fatal(err)
	}
	if _, exists := compose.Services["registry"]; exists {
		t.Fatal("registryImage started a bundled registry despite an external registry.url")
	}
	settings.RegistryImage = "https://jfrog.corp/dockerhub/library/registry:3"
	if _, err := renderDockerBundle(settings); err == nil || !strings.Contains(err.Error(), "registryImage") {
		t.Fatalf("invalid registryImage must fail before installation: %v", err)
	}
}

func TestProxyConfigurationIsPrivateInBothBundles(t *testing.T) {
	settings := &config.InstallFile{
		Image: "registry.example.com/donkey-server:test", Domain: "apps.example.com",
		Registry:      config.InstallRegistry{Registry: config.Registry{URL: "registry.example.com/donkey"}},
		Auth:          config.InstallAuth{Issuer: "https://id.example.com"},
		ProxyServices: config.ProxyServices{{Name: "erp", TargetURL: "https://user:private-password@enterprise.local/base", ProxyURL: "http://proxy:pa$$word@proxy.local:3128"}},
	}
	for _, render := range []func(*config.InstallFile) (map[string]bundleFile, error){renderKubernetesBundle, renderDockerBundle} {
		files, err := render(settings)
		if err != nil {
			t.Fatal(err)
		}
		private := files["proxy-services.json"]
		if private.Mode != 0o600 || string(private.Data) != settings.ProxyServices.JSON() {
			t.Fatal("proxy credentials must be preserved in a private file")
		}
		for name, file := range files {
			if name != "proxy-services.json" && (strings.Contains(string(file.Data), "private-password") || strings.Contains(string(file.Data), "pa$$word")) {
				t.Fatalf("proxy credential leaked into %s", name)
			}
		}
		if manifest, ok := files["manifests.yaml"]; ok {
			if !strings.Contains(string(manifest.Data), "name: donkey-proxy") || !strings.Contains(string(files["apply.sh"].Data), `--from-file=services.json="$bundle_dir/proxy-services.json"`) {
				t.Fatal("Kubernetes proxy deployment or configuration Secret is missing")
			}
		} else {
			var compose composeFile
			if err := yaml.Unmarshal(files["compose.yaml"].Data, &compose); err != nil {
				t.Fatal(err)
			}
			proxy := compose.Services["proxy"]
			if proxy.ContainerName != dockerProxyName || proxy.Environment["DONKEY_PROXY_SERVICES_FILE"] != dockerProxyConfigPath || len(proxy.Ports) != 0 {
				t.Fatal("Compose proxy is missing or publicly exposed")
			}
		}
	}
}

func TestKubernetesRenderOmitsEverySecretValue(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "registry-password")
	const registryPassword = "registry-password-must-not-render"
	if err := os.WriteFile(passwordFile, []byte(registryPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := &config.InstallFile{
		Runtime: "kubernetes",
		Image:   "registry.example.com/donkey-server:test",
		Registry: config.InstallRegistry{Registry: config.Registry{
			URL: "registry.example.com/donkey", Username: "builder", PasswordFile: passwordFile,
		}},
		Domain: "apps.example.com",
		Auth: config.InstallAuth{
			Issuer: "https://id.example.com", ClientID: "donkey", ClientSecret: "oidc-secret-must-not-render",
		},
		AI: config.InstallAI{
			BaseURL: "https://api.openai.com/v1", Model: "gpt-5.6-sol", APIKey: "openai-key-must-not-render",
		},
		Exa: config.InstallExa{BaseURL: "https://search.example.com/exa", APIKey: "exa-key-must-not-render"},
		Build: config.InstallBuild{
			BuildSettings: config.BuildSettings{
				Env:          map[string]string{"PRIVATE_INDEX": "build-env-secret-must-not-render"},
				Instructions: "internal-builder-instructions-must-not-render",
			},
			Proxy: config.InstallProxy{HTTPS: "http://proxy-user:proxy-secret-must-not-render@proxy.example.com:3128"},
			Packages: config.InstallPackages{
				CredentialHosts: []string{"registry.npmjs.org"}, Username: "package-reader", Password: "package-secret-must-not-render",
			},
		},
	}
	if err := validateKubernetesSettings(settings); err != nil {
		t.Fatal(err)
	}
	files, err := renderKubernetesBundle(settings)
	if err != nil {
		t.Fatal(err)
	}
	all := bundleText(files)
	for _, forbidden := range []string{
		registryPassword,
		settings.Auth.ClientSecret,
		settings.AI.APIKey, settings.Exa.APIKey,
		settings.Build.Env["PRIVATE_INDEX"],
		settings.Build.Instructions,
		settings.Build.Proxy.HTTPS,
		settings.Build.Packages.Password,
		"rendered-externally",
	} {
		if strings.Contains(all, forbidden) {
			t.Errorf("rendered bundle contains secret value %q", forbidden)
		}
	}
	manifest := string(files["manifests.yaml"].Data)
	if strings.Contains(manifest, "kind: Secret") {
		t.Fatal("Kubernetes bundle contains a Secret object")
	}
	if !strings.Contains(manifest, "donkey.local/installation: donkey-system") {
		t.Fatal("rendered objects do not carry the installation identity")
	}
	if !strings.Contains(manifest, "name: donkey-registry") {
		t.Fatal("credentialed system images do not reference the external pull Secret")
	}
	if !strings.Contains(manifest, "name: EXA_BASE_URL") || !strings.Contains(manifest, settings.Exa.BaseURL) || !strings.Contains(manifest, "key: EXA_API_KEY") {
		t.Fatal("Exa endpoint or credential reference missing")
	}
	apply := string(files["apply.sh"].Data)
	for _, name := range []string{"OPENAI_API_KEY", "EXA_API_KEY", "DONKEY_SESSION_KEY", "DONKEY_TELEMETRY_KEY", "donkey"} {
		if !strings.Contains(apply, name) {
			t.Errorf("apply helper does not name %s", name)
		}
	}
}

func TestKubernetesCertManagerAndExternalDNSRenderDirectGateway(t *testing.T) {
	settings := &config.InstallFile{
		Image:  "registry.example.com/donkey/server:test",
		Domain: "apps.example.com",
		Registry: config.InstallRegistry{Registry: config.Registry{
			URL: "registry.example.com/donkey",
		}},
		Auth: config.InstallAuth{Issuer: "https://id.example.com"},
		TLS:  config.InstallTLS{Mode: "cert-manager", IssuerName: "platform-ca"},
		Gateway: config.InstallGateway{ExternalDNS: config.InstallExternalDNS{
			Targets: []string{"10.204.166.5"},
		}},
	}
	if err := validateKubernetesSettings(settings); err != nil {
		t.Fatalf("direct cert-manager gateway should validate: %v", err)
	}
	files, err := renderKubernetesBundle(settings)
	if err != nil {
		t.Fatalf("rendering direct gateway: %v", err)
	}
	manifest := string(files["manifests.yaml"].Data)
	for _, want := range []string{"kind: Certificate", "kind: DNSEndpoint", "dnsName: apps.example.com", "secretName: donkey-gateway-tls", "type: LoadBalancer"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("rendered direct gateway installation is missing %q", want)
		}
	}
	if err := validateDockerSettings(settings); err == nil || !strings.Contains(err.Error(), "Kubernetes-only") {
		t.Fatalf("Docker should reject cert-manager/DNSEndpoint settings, got %v", err)
	}
}

func TestDockerRenderMatchesRuntimeTopologyWithoutSecrets(t *testing.T) {
	settings := &config.InstallFile{
		Runtime: "docker",
		Image:   "ghcr.io/example/donkey-server:test",
		Domain:  "apps.localhost",
		TLS:     config.InstallTLS{Mode: "ca"},
		Auth: config.InstallAuth{
			Issuer: "https://id.example.com", ClientID: "donkey", ClientSecret: "docker-oidc-secret-must-not-render",
		},
		AI: config.InstallAI{
			BaseURL: "https://api.openai.com/v1", Model: "gpt-5.6-sol", APIKey: "docker-openai-key-must-not-render",
		},
		Exa: config.InstallExa{BaseURL: "http://host.docker.internal:4242/exa", APIKey: "docker-exa-key-must-not-render"},
		Build: config.InstallBuild{
			BuildSettings:   config.BuildSettings{Env: map[string]string{"TOKEN": "docker-build-secret-must-not-render"}},
			RegistryMirrors: map[string]string{"docker.io": "https://mirror.example.com/dockerhub"},
			Proxy:           config.InstallProxy{HTTPS: "http://proxy-secret-must-not-render@example.com:3128"},
		},
	}
	files, err := renderDockerBundle(settings)
	if err != nil {
		t.Fatal(err)
	}
	all := bundleText(files)
	for _, forbidden := range []string{
		settings.Auth.ClientSecret, settings.AI.APIKey, settings.Exa.APIKey, settings.Build.Env["TOKEN"], settings.Build.Proxy.HTTPS,
	} {
		if strings.Contains(all, forbidden) {
			t.Errorf("Compose bundle contains secret value %q", forbidden)
		}
	}
	compose := string(files["compose.yaml"].Data)
	for _, want := range []string{
		"container_name: donkey-registry", "container_name: donkey-buildkitd", "container_name: donkey-packages",
		"container_name: donkey-telemetry", "container_name: donkey-server", "container_name: donkey-gateway",
		"container_name: donkey-ai", "DONKEY_PACKAGES_URL: http://${DONKEY_PACKAGES_IP:-172.31.250.10}:8090",
		"name: donkey-data", "name: donkey-buildkit", "name: donkey-telemetry", "name: donkey-registry",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("Compose bundle is missing %q", want)
		}
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(compose), &parsed); err != nil {
		t.Fatalf("compose.yaml is not YAML: %v", err)
	}
	var typed composeFile
	if err := yaml.Unmarshal([]byte(compose), &typed); err != nil {
		t.Fatalf("compose.yaml does not match the renderer schema: %v", err)
	}
	if typed.Services["ai"].Environment["EXA_BASE_URL"] != settings.Exa.BaseURL || typed.Services["ai"].Environment["EXA_API_KEY"] != "${EXA_API_KEY:-}" {
		t.Fatal("Exa settings missing from the proxy")
	}
	if _, found := typed.Services["server"].Environment["EXA_API_KEY"]; found {
		t.Fatal("Exa upstream key reached the admin API")
	}
	// Every compose service must carry exactly the role the direct
	// installer (and the bundle's own preflight) expects for its container —
	// the compose service key is not that contract.
	installRoles := map[string]string{}
	for _, item := range dockerInstallContainers(true) {
		installRoles[item.name] = item.role
	}
	for key, service := range typed.Services {
		role, ok := installRoles[service.ContainerName]
		if !ok {
			t.Errorf("Compose service %s container %s is not in the installer's owned set", key, service.ContainerName)
			continue
		}
		if got := service.Labels; !reflect.DeepEqual(got, runtimedocker.SystemLabels(role)) {
			t.Errorf("Compose service %s labels=%v, want SystemLabels(%q)", key, got, role)
		}
	}
	if got := typed.Networks["donkey"].Labels; !reflect.DeepEqual(got, runtimedocker.SystemLabels(runtimedocker.RoleNetwork)) {
		t.Errorf("Compose network labels=%v", got)
	}
	if got := typed.Services["server"].Environment["DONKEY_REGISTRY_MIRRORS"]; got != `{"docker.io":"https://mirror.example.com/dockerhub"}` {
		t.Errorf("server mirror map = %q", got)
	}
	// Builds run in the orchestrator: it gets the server's build path, the
	// Docker socket and the state volume, drains before Compose kills it,
	// and is the address the server forwards to.
	orchestrator := typed.Services["orchestrator"]
	if orchestrator.ContainerName != dockerOrchestratorName || !reflect.DeepEqual(orchestrator.Command, []string{"orchestrator"}) {
		t.Fatalf("orchestrator service = %+v", orchestrator)
	}
	if got := typed.Services["server"].Environment["DONKEY_ORCHESTRATOR_URL"]; got != "http://donkey-orchestrator:8087" {
		t.Errorf("server forwards to %q", got)
	}
	for key, want := range map[string]string{
		"DONKEY_LISTEN":           ":8087",
		"DONKEY_BUILDKIT_ADDR":    "tcp://donkey-buildkitd:1234",
		"DONKEY_SESSION_KEY":      "${DONKEY_SESSION_KEY:-}",
		"OPENAI_API_KEY":          "${OPENAI_API_KEY:-}",
		"DONKEY_REGISTRY_MIRRORS": `{"docker.io":"https://mirror.example.com/dockerhub"}`,
		"DONKEY_PACKAGES_URL":     "http://${DONKEY_PACKAGES_IP:-172.31.250.10}:8090",
	} {
		if got := orchestrator.Environment[key]; got != want {
			t.Errorf("orchestrator %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"DONKEY_OIDC_CLIENT_SECRET", "DONKEY_OIDC_CLIENT_ID", "DONKEY_ORCHESTRATOR_URL"} {
		if _, found := orchestrator.Environment[key]; found {
			t.Errorf("the orchestrator should not get %s", key)
		}
	}
	if !slices.Contains(orchestrator.Volumes, "/var/run/docker.sock:/var/run/docker.sock") ||
		!slices.Contains(orchestrator.Volumes, "data:"+dockerDataDir) || len(orchestrator.Ports) != 0 {
		t.Errorf("orchestrator volumes=%v ports=%v", orchestrator.Volumes, orchestrator.Ports)
	}
	if orchestrator.StopGracePeriod != "660s" {
		t.Errorf("orchestrator stop_grace_period = %q, want the drain window", orchestrator.StopGracePeriod)
	}
	if _, waits := orchestrator.DependsOn["buildkit"]; !waits {
		t.Error("the orchestrator should start after the build engine")
	}
	if got := string(files["buildkitd.toml"].Data); !strings.Contains(got, `mirrors = ["mirror.example.com/dockerhub"]`) {
		t.Errorf("BuildKit mirror config is missing from Docker bundle:\n%s", got)
	}
	for name, volume := range typed.Volumes {
		if volume.Labels["local.donkey.managed"] != "true" ||
			volume.Labels["local.donkey.installation"] != "donkey" || volume.Labels["local.donkey.role"] == "" {
			t.Errorf("Compose volume %s labels=%v", name, volume.Labels)
		}
	}
	up := string(files["up.sh"].Data)
	for _, want := range []string{
		"require_owned network donkey network",
		"require_owned volume donkey-data state-storage",
		"require_owned volume donkey-registry registry-storage",
		"require_owned container donkey-server server",
		"require_owned container donkey-buildkitd buildkitd",
		"require_owned container donkey-registry registry",
		"local.donkey.managed", "local.donkey.installation", "local.donkey.space",
		"local.donkey.app", "local.donkey.addon", "local.donkey.role",
		// Recovered env values are read with a non-line delimiter: a
		// line-based read truncated multi-line builder instructions.
		`awk -v RS="$(printf '\037')"`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("Compose ownership preflight is missing %q", want)
		}
	}
	if strings.Index(up, "require_owned container donkey-server server") > strings.Index(up, "existing donkey-server DONKEY_SESSION_KEY") {
		t.Error("Compose must validate ownership before reading an existing container's secrets")
	}
	drain := strings.Index(up, "docker stop --time 660 donkey-orchestrator")
	if !strings.Contains(up, "require_owned container donkey-orchestrator orchestrator") ||
		drain < 0 || drain > strings.Index(up, "docker compose -f") {
		t.Error("up.sh must let a verified orchestrator drain before Compose recreates the build engine")
	}
	check := exec.Command("sh", "-n")
	check.Stdin = strings.NewReader(up)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("generated up.sh is not valid POSIX shell: %v\n%s", err, output)
	}
}

func TestServerRenderWritesBundleAndRefusesOverwrite(t *testing.T) {
	t.Setenv("DONKEY_PACKAGES_PASSWORD", "ambient-package-secret-must-not-render")
	t.Setenv("DONKEY_OIDC_CLIENT_SECRET", "ambient-oidc-secret-must-not-render")
	t.Setenv("OPENAI_API_KEY", "ambient-openai-secret-must-not-render")
	output := filepath.Join(t.TempDir(), "deploy")
	args := []string{
		"donkey", "server", "render", "--runtime", "docker",
		"--image", "ghcr.io/example/donkey-server:test",
		"--domain", "apps.localhost", "--oidc-issuer", "https://id.example.com",
		"--output", output,
	}
	root := Command(Options{})
	root.Writer = &bytes.Buffer{}
	if err := root.Run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", "buildkitd.toml", "secrets.env.example", "up.sh", "README.md"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	for _, secret := range []string{
		"ambient-package-secret-must-not-render",
		"ambient-oidc-secret-must-not-render",
		"ambient-openai-secret-must-not-render",
	} {
		for _, name := range []string{"compose.yaml", "buildkitd.toml", "secrets.env.example", "up.sh", "README.md"} {
			raw, err := os.ReadFile(filepath.Join(output, name))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s contains ambient secret %q", name, secret)
			}
		}
	}
	root = Command(Options{})
	root.Writer = &bytes.Buffer{}
	if err := root.Run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second render should require --force, got %v", err)
	}
	secretFile := filepath.Join(output, "secrets.env")
	if err := os.WriteFile(secretFile, []byte("OPENAI_API_KEY=keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root = Command(Options{})
	root.Writer = &bytes.Buffer{}
	if err := root.Run(context.Background(), append(args, "--force")); err != nil {
		t.Fatalf("forced render: %v", err)
	}
	if raw, err := os.ReadFile(secretFile); err != nil || string(raw) != "OPENAI_API_KEY=keep-me\n" {
		t.Fatalf("forced render changed the user's secrets.env: %q, %v", raw, err)
	}
}

func bundleText(files map[string]bundleFile) string {
	var b strings.Builder
	for _, file := range files {
		b.Write(file.Data)
	}
	return b.String()
}
