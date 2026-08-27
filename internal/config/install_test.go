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
    routeNamespace: gateway-infra
    issuerName: company-pki
build:
  instructions: |
    Use base images under registry.example.com/mirror only.
    The library prefix contains Docker Hub official images.
  registryMirrors:
    docker.io: https://artifactory.example.com/docker-remote
  env:
    NPM_CONFIG_REGISTRY: https://artifactory.example.com/api/npm/npm
  proxy:
    https: http://proxy.example.com:3128
    noProxy: .example.com,10.0.0.0/8
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
		f.Gateway.AppGateway.RouteNamespace != "gateway-infra" ||
		f.Gateway.AppGateway.IssuerName != "company-pki" {
		t.Errorf("gateway.appGateway = %+v", f.Gateway.AppGateway)
	}
	if got := f.Build.Env["NPM_CONFIG_REGISTRY"]; got != "https://artifactory.example.com/api/npm/npm" {
		t.Errorf("env[NPM_CONFIG_REGISTRY] = %q", got)
	}
	if !strings.Contains(f.Build.Instructions, "registry.example.com/mirror") {
		t.Errorf("build instructions did not survive: %q", f.Build.Instructions)
	}
	if f.Build.Proxy.HTTPS != "http://proxy.example.com:3128" || f.Build.Proxy.NoProxy != ".example.com,10.0.0.0/8" {
		t.Errorf("proxy = %+v", f.Build.Proxy)
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
	appGateway.ClassName = "lgt-lb-standard"
	if err := appGateway.Validate(); err == nil {
		t.Error("a GatewayClass without an issuer cannot provision TLS")
	}
	appGateway.IssuerName = "lgt"
	if err := appGateway.Validate(); err != nil {
		t.Errorf("class, parent namespace and issuer are the intended setup: %v", err)
	}
	if !appGateway.Enabled() || appGateway.CertificateIssuerKind() != "ClusterIssuer" || appGateway.CertificateIssuerGroup() != "cert-manager.io" {
		t.Errorf("appGateway defaults not applied: %+v", appGateway)
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
		if err := f.Gateway.HTTPRoute.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.Gateway.AppGateway.Validate(); err != nil {
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
