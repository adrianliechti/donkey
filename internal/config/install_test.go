package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The build section reaches donkey-server as BuildSettings, embedded inline
// in InstallBuild — so it has to read back as one flat block, not as a
// nested "buildSettings" key nobody writes.
func TestInstallFileBuildRoundTrip(t *testing.T) {
	const in = `
image: ghcr.io/example/donkey-server:latest
domain: example.com
registry:
  url: registry.example.com/donkey
addonImages:
  postgres: registry.example.com/mirror/library/postgres
  rustfs: registry.example.com/mirror/rustfs/rustfs
addonImagePullSecrets:
  - registry-example-com
gateway:
  appGateway:
    className: private-edge
    nameTemplate: "$namespace-$hash"
    routeNamespace: gateway-infra
    issuerName: company-pki
build:
  instructions: |
    Use base images under registry.example.com/mirror only.
    The library prefix contains Docker Hub official images.
  registryMirrors:
    docker.io: https://artifactory.example.com/docker-remote
  env:
    CGO_ENABLED: "0"
  proxy:
    https: http://proxy.example.com:3128
    noProxy: .example.com,10.0.0.0/8
  packages:
    npm: https://artifactory.example.com/api/npm/npm
    pypi: https://artifactory.example.com/api/pypi/pypi/simple
    go: https://artifactory.example.com/api/go/go
    maven: https://artifactory.example.com/maven-remote
    nuget: https://artifactory.example.com/api/nuget/v3/nuget/index.json
    resourceHosts:
      - artifacts.example.com
    credentialHosts:
      - artifactory.example.com
    dockerConfigSecret: oci-mirror-registry-example-com
    dockerConfigHost: oci.mirror.registry.example.com
  verify: false
  verifySeconds: 90
  attempts: 2
`
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	// UnmarshalStrict: an unknown key here is a typo an admin has to hear
	// about, so every field below has to actually be spelled that way.
	f, err := LoadInstallFile(path)
	if err != nil {
		t.Fatalf("LoadInstallFile: %v", err)
	}
	if got := f.Build.RegistryMirrors["docker.io"]; got != "https://artifactory.example.com/docker-remote" {
		t.Errorf("registryMirrors[docker.io] = %q", got)
	}
	if f.AddonImages.Postgres != "registry.example.com/mirror/library/postgres" ||
		f.AddonImages.RustFS != "registry.example.com/mirror/rustfs/rustfs" {
		t.Errorf("addonImages = %+v", f.AddonImages)
	}
	if len(f.AddonImagePullSecrets) != 1 || f.AddonImagePullSecrets[0] != "registry-example-com" {
		t.Errorf("addonImagePullSecrets = %v", f.AddonImagePullSecrets)
	}
	if f.Gateway.AppGateway.ClassName != "private-edge" ||
		f.Gateway.AppGateway.NameTemplate != "$namespace-$hash" ||
		f.Gateway.AppGateway.RouteNamespace != "gateway-infra" ||
		f.Gateway.AppGateway.IssuerName != "company-pki" {
		t.Errorf("gateway.appGateway = %+v", f.Gateway.AppGateway)
	}
	if got := f.Build.Env["CGO_ENABLED"]; got != "0" {
		t.Errorf("env[CGO_ENABLED] = %q", got)
	}
	if !strings.Contains(f.Build.Instructions, "registry.example.com/mirror") {
		t.Errorf("build instructions did not survive: %q", f.Build.Instructions)
	}
	if f.Build.Proxy.HTTPS != "http://proxy.example.com:3128" || f.Build.Proxy.NoProxy != ".example.com,10.0.0.0/8" {
		t.Errorf("proxy = %+v", f.Build.Proxy)
	}
	if f.Build.Packages.Go != "https://artifactory.example.com/api/go/go" ||
		f.Build.Packages.Maven != "https://artifactory.example.com/maven-remote" ||
		f.Build.Packages.NuGet != "https://artifactory.example.com/api/nuget/v3/nuget/index.json" ||
		len(f.Build.Packages.ResourceHosts) != 1 || f.Build.Packages.ResourceHosts[0] != "artifacts.example.com" ||
		len(f.Build.Packages.CredentialHosts) != 1 || f.Build.Packages.CredentialHosts[0] != "artifactory.example.com" ||
		f.Build.Packages.DockerConfigSecret != "oci-mirror-registry-example-com" ||
		f.Build.Packages.DockerConfigHost != "oci.mirror.registry.example.com" {
		t.Errorf("packages = %+v", f.Build.Packages)
	}
	if f.Build.VerifyStartup() || f.Build.VerifyWindow() != 90 || f.Build.BuildAttempts() != 2 {
		t.Errorf("builder settings did not survive: %+v", f.Build.BuildSettings)
	}

	// And back out again in the same shape.
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var again InstallFile
	if err := yaml.UnmarshalStrict(out, &again); err != nil {
		t.Fatalf("what donkey writes, donkey cannot read: %v\n%s", err, out)
	}
	if again.Build.VerifyWindow() != f.Build.VerifyWindow() {
		t.Errorf("the build section did not survive the round trip:\n%s", out)
	}
}

func TestInstallBuildValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   InstallBuild
		wantErr bool
	}{
		{"empty", InstallBuild{}, false},
		{"negative attempts", InstallBuild{BuildSettings: BuildSettings{Attempts: -1}}, true},
		{"negative verify seconds", InstallBuild{BuildSettings: BuildSettings{VerifySeconds: -1}}, true},
		{"instructions too long", InstallBuild{BuildSettings: BuildSettings{Instructions: strings.Repeat("x", MaxBuilderInstructionsBytes+1)}}, true},
		{"mirror with no host", InstallBuild{RegistryMirrors: map[string]string{"": "https://mirror"}}, true},
		{"mirror with no address", InstallBuild{RegistryMirrors: map[string]string{"docker.io": ""}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestInstallPackagesDefaultsAndCredentialValidation(t *testing.T) {
	plain := InstallPackages{}
	if plain.NPMUpstream() != DefaultNPMUpstream || plain.PyPIUpstream() != DefaultPyPIUpstream || plain.GoUpstream() != DefaultGoUpstream || plain.MavenUpstream() != DefaultMavenUpstream || plain.NuGetUpstream() != DefaultNuGetUpstream {
		t.Fatalf("public defaults = %+v", plain)
	}
	if err := plain.ValidateCredentials(); err != nil {
		t.Fatalf("public defaults should need no credentials: %v", err)
	}

	valid := InstallPackages{
		NPM: "https://artifactory.example.com/npm", PyPI: "https://artifactory.example.com/pypi", Go: "https://artifactory.example.com/go",
		Maven: "https://artifactory.example.com/maven", NuGet: "https://artifactory.example.com/nuget/v3/index.json",
		CredentialHosts: []string{"artifactory.example.com"}, Username: "reader", Password: "token",
	}
	if err := valid.ValidateCredentials(); err != nil {
		t.Fatalf("valid enterprise package settings: %v", err)
	}
	multiHost := InstallPackages{
		NPM: "https://npm.mirror.example.com/npm", PyPI: "https://pypi.mirror.example.com/index/simple",
		CredentialHosts: []string{"npm.mirror.example.com", "pypi.mirror.example.com"},
		Username:        "reader", Password: "token",
	}
	if err := multiHost.ValidateCredentials(); err != nil {
		t.Fatalf("shared credentials across explicit mirror hosts: %v", err)
	}
	dockerConfig := InstallPackages{
		NPM: "https://packages.example.com/npm", ResourceHosts: []string{"artifacts.example.com"},
		CredentialHosts:    []string{"packages.example.com", "artifacts.example.com"},
		DockerConfigSecret: "package-registry-auth",
		DockerConfigHost:   "oci.mirror.example.com",
	}
	if err := dockerConfig.ValidateCredentials(); err != nil {
		t.Fatalf("Docker config package credentials: %v", err)
	}
	if dockerConfig.NeedsPassword() {
		t.Fatal("Docker config credentials should not recover an explicit password")
	}
	for _, bad := range []InstallPackages{
		{NPM: "registry.npmjs.org"},
		{NPM: "https://user:token@registry.npmjs.org"},
		{CredentialHosts: []string{"unrelated.example.com"}, Username: "reader", Password: "token"},
		{CredentialHosts: []string{"registry.npmjs.org"}, Username: "reader"},
		{CredentialHosts: []string{"registry.npmjs.org", "REGISTRY.NPMJS.ORG"}, Username: "reader", Password: "token"},
		{NPM: "http://registry.npmjs.org", CredentialHosts: []string{"registry.npmjs.org"}, Username: "reader", Password: "token"},
		{ResourceHosts: []string{"artifacts.example.com"}, CredentialHosts: []string{"artifacts.example.com"}},
		{CredentialHosts: []string{"registry.npmjs.org"}, DockerConfigSecret: "auth", DockerConfigDir: "/auth", DockerConfigHost: "oci.example.com"},
		{CredentialHosts: []string{"registry.npmjs.org"}, Username: "reader", Password: "token", DockerConfigSecret: "auth", DockerConfigHost: "oci.example.com"},
		{DockerConfigSecret: "auth"},
		{CredentialHosts: []string{"registry.npmjs.org"}, DockerConfigSecret: "auth"},
		{CredentialHosts: []string{"registry.npmjs.org"}, DockerConfigHost: "oci.example.com"},
		{CredentialHosts: []string{"registry.npmjs.org"}, DockerConfigSecret: "auth", DockerConfigHost: "https://oci.example.com"},
		{ResourceHosts: []string{"bad host"}},
		{ResourceHosts: []string{"artifacts.example.com"}, CredentialHosts: []string{"artifacts.example.com"}, Username: "reader"},
	} {
		if err := bad.ValidateCredentials(); err == nil {
			t.Errorf("invalid package settings passed: %+v", bad)
		}
	}
	if err := dockerConfig.ValidateRuntime("kubernetes"); err != nil {
		t.Fatalf("Kubernetes Docker config Secret: %v", err)
	}
	dockerDir := dockerConfig
	dockerDir.DockerConfigSecret = ""
	dockerDir.DockerConfigDir = "/run/package-auth"
	if err := dockerDir.ValidateRuntime("docker"); err != nil {
		t.Fatalf("Docker config directory: %v", err)
	}
	for _, invalid := range []struct {
		packages InstallPackages
		runtime  string
	}{
		{dockerConfig, "docker"},
		{dockerDir, "kubernetes"},
		{InstallPackages{DockerConfigDir: "relative", DockerConfigHost: "oci.example.com"}, "docker"},
		{InstallPackages{DockerConfigSecret: "Bad_Name", DockerConfigHost: "oci.example.com"}, "kubernetes"},
	} {
		if err := invalid.packages.ValidateRuntime(invalid.runtime); err == nil {
			t.Errorf("invalid %s package credential source passed: %+v", invalid.runtime, invalid.packages)
		}
	}
}

func TestInstallAddonImagesValidate(t *testing.T) {
	valid := InstallAddonImages{
		Postgres: "registry.example.com:5000/mirror/library/postgres",
		RustFS:   "registry.example.com/mirror/rustfs/rustfs",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid repository overrides were refused: %v", err)
	}
	if got := valid.Repository("postgres"); got != valid.Postgres {
		t.Fatalf("postgres repository = %q", got)
	}
	for _, bad := range []InstallAddonImages{
		{Postgres: "https://registry.example.com/postgres"},
		{Postgres: "registry.example.com/postgres:16"},
		{RustFS: "registry.example.com/rustfs@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("invalid addon image override passed: %+v", bad)
		}
	}
}

func TestValidateAddonImagePullSecrets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets []string
		wantErr bool
	}{
		{name: "empty list"},
		{name: "dns labels", secrets: []string{"registry-example-com", "platform.registry-auth"}},
		{name: "empty name", secrets: []string{""}, wantErr: true},
		{name: "uppercase", secrets: []string{"Registry-Auth"}, wantErr: true},
		{name: "underscore", secrets: []string{"registry_auth"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAddonImagePullSecrets(tc.secrets); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAddonImagePullSecrets(%v) err = %v, wantErr %v", tc.secrets, err, tc.wantErr)
			}
		})
	}
}

func TestInstallRegistryCredentialSourceMatchesRuntime(t *testing.T) {
	kubernetes := InstallRegistry{DockerConfigSecret: "registry-mirror-auth"}
	if err := kubernetes.ValidateRuntime("kubernetes"); err != nil {
		t.Fatal(err)
	}
	docker := InstallRegistry{Registry: Registry{DockerConfigDir: "/run/registry-auth"}}
	if err := docker.ValidateRuntime("docker"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		registry InstallRegistry
		runtime  string
	}{
		{kubernetes, "docker"},
		{docker, "kubernetes"},
		{InstallRegistry{Registry: Registry{DockerConfigDir: "relative"}}, "docker"},
		{InstallRegistry{DockerConfigSecret: "Bad_Name"}, "kubernetes"},
	} {
		if err := invalid.registry.ValidateRuntime(invalid.runtime); err == nil {
			t.Errorf("invalid %s registry credential source passed: %+v", invalid.runtime, invalid.registry)
		}
	}
}

// An installation's build variables are the fallback; an app that pins its
// own has a reason to.
func TestBuildSettingsMergeEnv(t *testing.T) {
	settings := BuildSettings{Env: map[string]string{
		"NPM_CONFIG_REGISTRY": "https://mirror.example.com/npm",
		"GOPROXY":             "https://mirror.example.com/go",
	}}
	got := settings.MergeEnv(map[string]string{
		"GOPROXY":  "direct",
		"NODE_ENV": "production",
	})
	for k, want := range map[string]string{
		"NPM_CONFIG_REGISTRY": "https://mirror.example.com/npm",
		"GOPROXY":             "direct",
		"NODE_ENV":            "production",
	} {
		if got[k] != want {
			t.Errorf("merged[%s] = %q, want %q", k, got[k], want)
		}
	}
	// The installation's own map must not be edited by a merge — the next
	// app's build reads it too.
	if settings.Env["GOPROXY"] != "https://mirror.example.com/go" {
		t.Error("merging an app's variables changed the installation's")
	}
	if got := settings.MergeEnv(nil); got["GOPROXY"] != "https://mirror.example.com/go" {
		t.Errorf("an app with no variables of its own lost the installation's: %v", got)
	}
}

func TestBuildSettingsDefaults(t *testing.T) {
	var s BuildSettings
	// Startup verification is on unless an installation says otherwise —
	// an unset pointer must not read as "off".
	if !s.VerifyStartup() {
		t.Error("startup verification should default to on")
	}
	if s.VerifyWindow() != DefaultVerifySeconds || s.BuildAttempts() != DefaultAttempts {
		t.Errorf("defaults not applied: %ds, %d attempts", s.VerifyWindow(), s.BuildAttempts())
	}
	s.Verify = new(false)
	if s.VerifyStartup() {
		t.Error("an explicit verify: false was ignored")
	}
}

// Both namespace settings mean the same thing: name it and it is yours,
// leave it empty and donkey makes its own.
func TestInstallKubernetesDefaults(t *testing.T) {
	var k InstallKubernetes
	if k.SystemNamespace() != DefaultSystemNamespace {
		t.Errorf("an unset system namespace should be %s, got %s", DefaultSystemNamespace, k.SystemNamespace())
	}
	if k.NamespacePrefix() != DefaultNamespacePrefix {
		t.Errorf("an unset prefix should be %s, got %s", DefaultNamespacePrefix, k.NamespacePrefix())
	}
	if err := k.Validate(); err != nil {
		t.Errorf("an empty block is the default installation: %v", err)
	}

	k.System.Namespace = "bank-donkey-system"
	k.Workload.Namespace = "bank-donkey-apps"
	if err := k.Validate(); err != nil {
		t.Errorf("two distinct namespaces are the intended setup: %v", err)
	}
	if k.SystemNamespace() != "bank-donkey-system" {
		t.Error("a configured system namespace should win")
	}

	// Space isolation is on unless an installation says otherwise — an
	// unset pointer must not read as "off".
	if !k.SpacesIsolated() {
		t.Error("space isolation should default to on")
	}
	k.IsolateSpaces = new(false)
	if k.SpacesIsolated() {
		t.Error("an explicit isolateSpaces: false was ignored")
	}
}

func TestInstallKubernetesRejectsUnworkableCombinations(t *testing.T) {
	// Apps next to a privileged build engine, inside the blast radius of
	// every destroy.
	same := InstallKubernetes{}
	same.System.Namespace = "donkey-x"
	same.Workload.Namespace = "donkey-x"
	if err := same.Validate(); err == nil {
		t.Error("one namespace for the control plane and the apps must be refused")
	}

	// A prefix maps a space onto a namespace; with one namespace for
	// everything there is nothing left to prefix.
	both := InstallKubernetes{}
	both.Workload.Namespace = "donkey-apps"
	both.Workload.NamespacePrefix = "donkey-"
	if err := both.Validate(); err == nil {
		t.Error("a prefix together with a shared namespace is a contradiction")
	}

	bad := InstallKubernetes{}
	bad.Workload.Namespace = "Donkey_Apps"
	if err := bad.Validate(); err == nil {
		t.Error("a name the cluster would reject must be caught before the install runs")
	}

	// Egress rules ride the isolation policy, so restricting egress without
	// isolation has nowhere to live.
	egress := InstallKubernetes{IsolateSpaces: new(false), RestrictEgress: true}
	if err := egress.Validate(); err == nil {
		t.Error("restrictEgress without isolateSpaces must be refused")
	}
	egress.IsolateSpaces = nil // default-on
	if err := egress.Validate(); err != nil {
		t.Errorf("restrictEgress with isolation on (the default) is valid: %v", err)
	}
}

// The optional attachment and the TLS block: naming half a gateway or an
// unknown mode are configurations that could only fail later and worse.
func TestInstallAttachmentValidation(t *testing.T) {
	var tls InstallTLS
	if err := tls.Validate(); err != nil {
		t.Errorf("an empty TLS block is the default installation: %v", err)
	}
	tls.Mode = "none"
	if err := tls.Validate(); err != nil {
		t.Errorf("none is the mode behind a TLS-terminating cluster gateway: %v", err)
	}
	// YAML 1.1 reads a bare `off` as a boolean, arriving here as "false" —
	// both spellings get pointed at the real name.
	for _, mode := range []string{"off", "false"} {
		tls.Mode = mode
		if err := tls.Validate(); err == nil || !strings.Contains(err.Error(), "none") {
			t.Errorf("mode %q should point at none, got %v", mode, err)
		}
	}
	tls = InstallTLS{Mode: "letsencrypt"}
	if err := tls.Validate(); err == nil {
		t.Error("an unknown tls.mode must be refused")
	}

	var route InstallHTTPRoute
	if err := route.Validate(); err != nil {
		t.Errorf("no httpRoute block is the default: %v", err)
	}
	route.Namespace = "infra"
	if err := route.Validate(); err == nil {
		t.Error("a namespace without a gateway name points at nothing")
	}
	route.Name = "shared-gateway"
	if err := route.Validate(); err != nil {
		t.Errorf("name and namespace are the intended setup: %v", err)
	}
	if !route.Enabled() {
		t.Error("a named gateway means the attachment is on")
	}

	var appGateway InstallAppGateway
	if err := appGateway.Validate(); err != nil {
		t.Errorf("no appGateway block is the default: %v", err)
	}
	appGateway.RouteNamespace = "projectcontour"
	if err := appGateway.Validate(); err == nil {
		t.Error("a route namespace without a GatewayClass points at nothing")
	}
	appGateway.ClassName = "platform"
	if err := appGateway.Validate(); err == nil {
		t.Error("a GatewayClass without an issuer cannot provision TLS")
	}
	appGateway.IssuerName = "platform-ca"
	if err := appGateway.Validate(); err != nil {
		t.Errorf("class, parent namespace and issuer are the intended setup: %v", err)
	}
	if !appGateway.Enabled() || appGateway.CertificateIssuerKind() != "ClusterIssuer" || appGateway.CertificateIssuerGroup() != "cert-manager.io" {
		t.Errorf("appGateway defaults not applied: %+v", appGateway)
	}
	appGateway.NameTemplate = "$unknown"
	if err := appGateway.Validate(); err == nil {
		t.Error("an unknown app Gateway name parameter should be rejected")
	}
	appGateway.NameTemplate = "$namespace-$hash"
	if err := appGateway.Validate(); err != nil {
		t.Errorf("documented app Gateway name template should be valid: %v", err)
	}

	var contour InstallContourHTTPProxy
	if err := contour.Validate(); err != nil {
		t.Errorf("no contourHTTPProxy block is the default: %v", err)
	}
	contour.IssuerName = "platform-ca"
	if err := contour.Validate(); err == nil {
		t.Error("a Contour issuer without an ingress class cannot select a data plane")
	}
	contour.IngressClassName = "contour.private"
	if err := contour.Validate(); err != nil {
		t.Errorf("ingress class and issuer are the intended Contour setup: %v", err)
	}
	if !contour.Enabled() || contour.CertificateIssuerKind() != "ClusterIssuer" || contour.CertificateIssuerGroup() != "cert-manager.io" {
		t.Errorf("Contour edge defaults not applied: %+v", contour)
	}
	contour.IngressClassName = "Not A Class"
	if err := contour.Validate(); err == nil {
		t.Error("an invalid Contour ingress class should be rejected")
	}

	contour.IngressClassName = "contour"
	modes := InstallGateway{AppGateway: appGateway, ContourHTTPProxy: contour}
	if !modes.UsesExternalTLS() {
		t.Error("a configured managed edge must report external TLS")
	}
	if err := modes.Validate(); err == nil || !strings.Contains(err.Error(), "alternative") {
		t.Errorf("two per-host edge modes should be rejected, got %v", err)
	}
	modes.AppGateway = InstallAppGateway{}
	modes.HTTPRoute = InstallHTTPRoute{Name: "shared"}
	if err := modes.Validate(); err == nil || !strings.Contains(err.Error(), "both expose") {
		t.Errorf("aggregate HTTPRoute and Contour hosts should be rejected, got %v", err)
	}
}

func TestContourHTTPProxyInstallFileRoundTrip(t *testing.T) {
	const input = `
gateway:
  contourHTTPProxy:
    ingressClassName: contour-private
    issuerName: company-pki
    issuerKind: ClusterIssuer
    issuerGroup: cert-manager.io
`
	var f InstallFile
	if err := yaml.UnmarshalStrict([]byte(input), &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Gateway.ContourHTTPProxy; got.IngressClassName != "contour-private" || got.IssuerName != "company-pki" {
		t.Fatalf("gateway.contourHTTPProxy = %+v", got)
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var again InstallFile
	if err := yaml.UnmarshalStrict(out, &again); err != nil {
		t.Fatalf("round trip: %v\n%s", err, out)
	}
	if again.Gateway.ContourHTTPProxy != f.Gateway.ContourHTTPProxy {
		t.Fatalf("round trip changed Contour settings: %+v", again.Gateway.ContourHTTPProxy)
	}
}

// The example install files are the first thing a person copies — they have
// to parse (strictly) and validate, or the copy fails before donkey runs.
func TestExampleInstallFilesLoad(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*install*.yaml")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no example install files found (%v)", err)
	}
	for _, m := range matches {
		f, err := LoadInstallFile(m)
		if err != nil {
			t.Errorf("%s: %v", m, err)
			continue
		}
		if err := f.TLS.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.Kubernetes.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.Gateway.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.Build.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.AddonImages.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		t.Logf("%s: tls=%q httpRoute=%v system=%q workload=%q",
			filepath.Base(m), f.TLS.Mode, f.Gateway.HTTPRoute.Enabled(),
			f.Kubernetes.System.Namespace, f.Kubernetes.Workload.Namespace)
	}
}
