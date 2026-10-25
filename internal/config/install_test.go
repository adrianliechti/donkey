package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// The build section reaches donkey-server as BuildSettings, embedded inline
// in InstallBuild — so it has to read back as one flat block, not as a
// nested "buildSettings" key nobody writes.
func TestInstallFileBuildRoundTrip(t *testing.T) {
	const in = `
image: ghcr.io/example/donkey-server:latest
busyboxImage: jfrog.corp/dockerhub/library/busybox:1.37
registryImage: jfrog.corp/dockerhub/library/registry:3
domain: example.com
registry:
  url: registry.example.com/donkey
addonImages:
  postgres: registry.example.com/mirror/library/postgres
  redis: registry.example.com/mirror/library/redis
  rustfs: registry.example.com/mirror/rustfs/rustfs
addonImagePullSecrets:
  - registry-example-com
gateway:
  externalDNS:
    targets: [10.204.166.5]
    ttl: 120
tls:
  mode: cert-manager
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
    debian: https://artifactory.example.com/debian
    alpine: https://artifactory.example.com/alpinelinux
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
	if f.BusyboxImage != "jfrog.corp/dockerhub/library/busybox:1.37" {
		t.Errorf("busyboxImage = %q", f.BusyboxImage)
	}
	if f.RegistryImage != "jfrog.corp/dockerhub/library/registry:3" {
		t.Errorf("registryImage = %q", f.RegistryImage)
	}
	if f.AddonImages.Postgres != "registry.example.com/mirror/library/postgres" ||
		f.AddonImages.Redis != "registry.example.com/mirror/library/redis" ||
		f.AddonImages.RustFS != "registry.example.com/mirror/rustfs/rustfs" {
		t.Errorf("addonImages = %+v", f.AddonImages)
	}
	if len(f.AddonImagePullSecrets) != 1 || f.AddonImagePullSecrets[0] != "registry-example-com" {
		t.Errorf("addonImagePullSecrets = %v", f.AddonImagePullSecrets)
	}
	if !f.TLS.UsesCertManager() || f.TLS.IssuerName != "company-pki" {
		t.Errorf("tls = %+v", f.TLS)
	}
	if !slices.Equal(f.Gateway.ExternalDNS.Targets, []string{"10.204.166.5"}) ||
		f.Gateway.ExternalDNS.EffectiveRecordType() != "A" || f.Gateway.ExternalDNS.TTL != 120 {
		t.Errorf("gateway.externalDNS = %+v", f.Gateway.ExternalDNS)
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
	if f.Build.Packages.Debian != "https://artifactory.example.com/debian" ||
		f.Build.Packages.Alpine != "https://artifactory.example.com/alpinelinux" ||
		f.Build.Packages.Go != "https://artifactory.example.com/api/go/go" ||
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
		{"long instructions", InstallBuild{BuildSettings: BuildSettings{Instructions: string(make([]byte, 256<<10))}}, false},
		{"mirror with no host", InstallBuild{RegistryMirrors: map[string]string{"": "https://mirror"}}, true},
		{"mirror with no address", InstallBuild{RegistryMirrors: map[string]string{"docker.io": ""}}, true},
		{"source repository path", InstallBuild{RegistryMirrors: map[string]string{"docker.io/library": "mirror.example.com/docker"}}, true},
		{"mirror credentials", InstallBuild{RegistryMirrors: map[string]string{"docker.io": "https://user:token@mirror.example.com/docker"}}, true},
		{"mirror query", InstallBuild{RegistryMirrors: map[string]string{"docker.io": "https://mirror.example.com/docker?token=secret"}}, true},
		{"unsupported mirror scheme", InstallBuild{RegistryMirrors: map[string]string{"docker.io": "ftp://mirror.example.com/docker"}}, true},
		{"url source tolerated", InstallBuild{RegistryMirrors: map[string]string{"https://docker.io/": "https://mirror.example.com/docker"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryMirrorNormalization(t *testing.T) {
	for raw, want := range map[string]string{
		"https://jfrog.corp/docker-remote": "jfrog.corp/docker-remote",
		"https://JFROG.CORP/docker-remote": "jfrog.corp/docker-remote",
		"jfrog.corp/docker-remote":         "jfrog.corp/docker-remote",
		"http://mirror.internal:5000":      "mirror.internal:5000",
		"  jfrog.corp/x/  ":                "jfrog.corp/x",
	} {
		got, err := ParseRegistryMirror(raw)
		if err != nil || got.Address != want {
			t.Errorf("ParseRegistryMirror(%q) = %+v, %v; want address %q", raw, got, err, want)
		}
	}
	httpMirror, _ := ParseRegistryMirror("http://mirror.internal:5000")
	if !httpMirror.PlainHTTP {
		t.Error("an http:// mirror was not recognized as plain http")
	}
	httpsMirror, _ := ParseRegistryMirror("https://jfrog.corp/x")
	if httpsMirror.PlainHTTP {
		t.Error("an https:// mirror was treated as plain http")
	}
	if source, err := NormalizeRegistrySource("https://DOCKER.IO/"); err != nil || source != "docker.io" {
		t.Errorf("Docker Hub source = %q, %v", source, err)
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
	kubernetes := InstallRegistry{CASecret: "registry-enterprise-ca", DockerConfigSecret: "registry-mirror-auth"}
	if err := kubernetes.ValidateRuntime("kubernetes"); err != nil {
		t.Fatal(err)
	}
	docker := InstallRegistry{Registry: Registry{CAFile: "/run/enterprise-ca.crt", DockerConfigDir: "/run/registry-auth"}}
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
		{InstallRegistry{CASecret: "Bad_Name"}, "kubernetes"},
		{InstallRegistry{CASecret: "enterprise-ca"}, "docker"},
		{InstallRegistry{Registry: Registry{CAFile: "/run/enterprise-ca.crt"}}, "kubernetes"},
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

func TestInstallTrafficValidation(t *testing.T) {
	var tls InstallTLS
	if err := tls.Validate(); err != nil {
		t.Errorf("an empty TLS block is the default installation: %v", err)
	}
	if tls.EffectiveMode() != "acme" {
		t.Errorf("empty TLS mode = %q, want acme", tls.EffectiveMode())
	}
	tls = InstallTLS{Mode: "cert-manager"}
	if err := tls.Validate(); err == nil || !strings.Contains(err.Error(), "issuerName") {
		t.Errorf("cert-manager without an issuer should fail clearly, got %v", err)
	}
	tls.IssuerName = "platform-ca"
	if err := tls.Validate(); err != nil {
		t.Errorf("cert-manager issuer should validate: %v", err)
	}
	if tls.CertificateIssuerKind() != "ClusterIssuer" || tls.CertificateIssuerGroup() != "cert-manager.io" {
		t.Errorf("cert-manager defaults not applied: %+v", tls)
	}
	for _, invalid := range []InstallTLS{
		{Mode: "none"},
		{Mode: "letsencrypt"},
		{IssuerName: "platform-ca"},
		{Mode: "ca", IssuerName: "platform-ca"},
		{Mode: "ca", Email: "ops@example.com"},
		{Mode: "cert-manager", IssuerName: "Not Valid"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid TLS settings passed: %+v", invalid)
		}
	}

	var dns InstallExternalDNS
	if err := dns.Validate(); err != nil {
		t.Errorf("external DNS is optional: %v", err)
	}
	dns = InstallExternalDNS{Targets: []string{"10.204.166.5"}, TTL: 60}
	if err := dns.Validate(); err != nil || dns.EffectiveRecordType() != "A" {
		t.Errorf("IPv4 VIP should infer an A record: type=%q err=%v", dns.EffectiveRecordType(), err)
	}
	dns = InstallExternalDNS{Targets: []string{"gateway.example.com."}}
	if err := dns.Validate(); err != nil || dns.EffectiveRecordType() != "CNAME" {
		t.Errorf("hostname should infer a CNAME: type=%q err=%v", dns.EffectiveRecordType(), err)
	}
	for _, invalid := range []InstallExternalDNS{
		{RecordType: "A"},
		{Targets: []string{"gateway.example.com"}, RecordType: "A"},
		{Targets: []string{"10.0.0.1"}, RecordType: "CNAME"},
		{Targets: []string{"10.0.0.1"}, TTL: -1},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid external DNS settings passed: %+v", invalid)
		}
	}
}

func TestExternalDNSInstallFileRoundTrip(t *testing.T) {
	const input = `
gateway:
  externalDNS:
    targets:
      - 10.204.166.5
    recordType: A
    ttl: 300
`
	var f InstallFile
	if err := yaml.UnmarshalStrict([]byte(input), &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Gateway.ExternalDNS; !slices.Equal(got.Targets, []string{"10.204.166.5"}) || got.RecordType != "A" || got.TTL != 300 {
		t.Fatalf("gateway.externalDNS = %+v", got)
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var again InstallFile
	if err := yaml.UnmarshalStrict(out, &again); err != nil {
		t.Fatalf("round trip: %v\n%s", err, out)
	}
	if got := again.Gateway.ExternalDNS; !slices.Equal(got.Targets, f.Gateway.ExternalDNS.Targets) || got.RecordType != "A" || got.TTL != 300 {
		t.Fatalf("round trip changed external DNS settings: %+v", got)
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
		if err := f.Gateway.ExternalDNS.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.Build.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		if err := f.AddonImages.Validate(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
		t.Logf("%s: tls=%q externalDNS=%v system=%q workload=%q",
			filepath.Base(m), f.TLS.EffectiveMode(), f.Gateway.ExternalDNS.Enabled(),
			f.Kubernetes.System.Namespace, f.Kubernetes.Workload.Namespace)
	}
}

func TestOSPackageMirrorsAreOptionalAndCredentialsAreScoped(t *testing.T) {
	p := InstallPackages{
		Debian: "https://debian.mirror.example/debian", Alpine: "https://alpine.mirror.example/alpinelinux",
		CredentialHosts: []string{"debian.mirror.example", "alpine.mirror.example"}, Username: "reader", Password: "token",
	}
	if err := p.ValidateCredentials(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"debian", "alpine"} {
		for _, raw := range []string{"ftp://mirror.example", "https://reader:token@mirror.example", "https://mirror.example/?token=secret"} {
			bad := InstallPackages{}
			if field == "debian" {
				bad.Debian = raw
			} else {
				bad.Alpine = raw
			}
			if err := bad.Validate(); err == nil {
				t.Errorf("accepted %s mirror %q", field, raw)
			}
		}
	}
	p.Debian = "http://debian.mirror.example/debian"
	if err := p.ValidateCredentials(); err == nil {
		t.Fatal("credentials over plaintext HTTP were accepted")
	}
}

func TestS3ServiceUsesRustFSImageMirror(t *testing.T) {
	var images InstallAddonImages
	if err := yaml.UnmarshalStrict([]byte("rustfs: mirror.example.com/rustfs/rustfs"), &images); err != nil {
		t.Fatal(err)
	}
	if err := images.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"s3", "rustfs"} {
		if got := images.Repository(typ); got != "mirror.example.com/rustfs/rustfs" {
			t.Fatalf("%s image repository = %q", typ, got)
		}
	}
}

// Scan settings default to the recommended policy, refuse values that would
// otherwise silently read as a default, and travel to the server as
// environment without losing anything.
func TestScanSettings(t *testing.T) {
	var zero ScanSettings
	if !zero.IsEnabled() || zero.ScannerImage() != DefaultScannerImage || zero.BlockSeverity() != ScanBlockCritical ||
		!zero.IgnoreUnfixedFindings() || zero.SecretsPolicy() != ScanSecretsBlock || !zero.AssistantTriage() {
		t.Errorf("zero value is not the recommended policy: %+v", zero)
	}
	if err := zero.Validate(); err != nil {
		t.Errorf("zero value must validate: %v", err)
	}
	if len(zero.EnvVars()) != 0 {
		t.Errorf("unset settings must travel as nothing, got %v", zero.EnvVars())
	}
	for name, bad := range map[string]ScanSettings{
		"severity":        {Block: "severe"},
		"secrets":         {Secrets: "maybe"},
		"triage":          {Triage: "human"},
		"scheme in repo":  {DBRepository: "https://jfrog.corp/ghcr/aquasecurity/trivy-db"},
		"bad image":       {Image: "Aquasec/Trivy:latest"},
		"bad java repo":   {JavaDBRepository: "not a reference!"},
		"digest as repo?": {DBRepository: "jfrog.corp/x@sha256:zzz"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: %+v validated", name, bad)
		}
	}
	good := ScanSettings{
		Enabled: new(true), Image: "jfrog.corp/dockerhub/aquasec/trivy:0.76.1@sha256:790fd66d1697395d8693d1acaa1ea702406a20f109b6fc7bac8db623eca6c06c",
		DBRepository: "jfrog.corp/ghcr/aquasecurity/trivy-db", JavaDBRepository: "jfrog.corp/ghcr/aquasecurity/trivy-java-db:1",
		Block: "HIGH", IgnoreUnfixed: new(false), Secrets: "warn", Triage: "off",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a mirrored, strict configuration must validate: %v", err)
	}
	if good.BlockSeverity() != ScanBlockHigh || good.IgnoreUnfixedFindings() || good.SecretsPolicy() != ScanSecretsWarn || good.AssistantTriage() {
		t.Errorf("accessors misread %+v", good)
	}
	env := good.EnvVars()
	if len(env) != 8 {
		t.Errorf("every set field travels: %v", env)
	}
	back := ScanSettingsFromEnv(func(key string) string { return env[key] })
	if !reflect.DeepEqual(back, good) {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", back, good)
	}
	if err := (InstallBuild{BuildSettings: BuildSettings{Scan: ScanSettings{Block: "x"}}}).Validate(); err == nil || !strings.Contains(err.Error(), "build.scan.block") {
		t.Errorf("build validation must cover the scan: %v", err)
	}
}

func TestPeriodicScanSettings(t *testing.T) {
	if (ScanSettings{}).RescanInterval() != 4*time.Hour {
		t.Fatal("rescan default is not four hours")
	}
	for _, value := range []string{"0", "1m", "4h", "24h"} {
		settings := ScanSettings{Rescan: value}
		if err := settings.Validate(); err != nil {
			t.Fatal(err)
		}
		env := settings.EnvVars()
		if restored := ScanSettingsFromEnv(func(key string) string { return env[key] }); restored.Rescan != value {
			t.Fatalf("lost rescan interval: %+v", restored)
		}
	}
	for _, value := range []string{"-1h", "30s", "tomorrow", "0s"} {
		if err := (ScanSettings{Rescan: value}).Validate(); err == nil {
			t.Fatalf("accepted interval %q", value)
		}
	}
	if (ScanSettings{Enabled: new(false)}).RescanInterval() != 0 || (ScanSettings{Rescan: "0"}).RescanInterval() != 0 {
		t.Fatal("disabled scanner still schedules work")
	}
}

// The server image carries the scanner binary the orchestrator matches
// with; it must be the release the build engine inventories with.
func TestServerImageCarriesTheScanner(t *testing.T) {
	raw, err := os.ReadFile("../../hack/Dockerfile.server")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FROM "+DefaultScannerImage+" AS scanner") {
		t.Errorf("hack/Dockerfile.server does not copy the scanner from %s", DefaultScannerImage)
	}
}
