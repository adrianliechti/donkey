package manifests

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/BurntSushi/toml"

	"github.com/adrianliechti/donkey/internal/config"
)

// buildkitd reads this file once, at startup, and refuses to start on a
// syntax error — so the shape is checked by parsing it, not by matching text.
func parseBuildkitTOML(t *testing.T, s string) map[string]any {
	t.Helper()
	var got map[string]any
	if _, err := toml.Decode(s, &got); err != nil {
		t.Fatalf("buildkitd would reject this config: %v\n%s", err, s)
	}
	return got
}

func TestBuildkitTOMLMirrors(t *testing.T) {
	got := BuildkitTOML(BuildkitConfig{
		RegistryHost:   "registry.example.com",
		RegistryCAFile: "/etc/donkey/registry-ca/ca.crt",
		Mirrors: map[string]string{
			"docker.io": "https://artifactory.example.com/docker-remote",
			"ghcr.io":   "artifactory.example.com/ghcr-remote",
		},
	})
	parsed := parseBuildkitTOML(t, got)

	registry, _ := parsed["registry"].(map[string]any)
	if registry == nil {
		t.Fatalf("no registry section:\n%s", got)
	}
	// The scheme is stripped: BuildKit's mirrors are hosts (optionally with a
	// path), and a URL there would be read as a host called "https:".
	for host, want := range map[string]string{
		"docker.io": "artifactory.example.com/docker-remote",
		"ghcr.io":   "artifactory.example.com/ghcr-remote",
	} {
		section, _ := registry[host].(map[string]any)
		if section == nil {
			t.Errorf("%s has no section:\n%s", host, got)
			continue
		}
		mirrors, _ := section["mirrors"].([]any)
		if len(mirrors) != 1 || mirrors[0] != want {
			t.Errorf("%s mirrors = %v, want [%s]", host, mirrors, want)
		}
	}

	// The mirror is a registry too, and an in-house one is behind the same
	// private CA as donkey's registry — declared once, though two mirror
	// entries point at it, because TOML rejects a table declared twice.
	mirror, _ := registry["artifactory.example.com"].(map[string]any)
	if mirror == nil {
		t.Fatalf("the mirror host has no section of its own:\n%s", got)
	}
	ca, _ := mirror["ca"].([]any)
	if len(ca) != 1 || ca[0] != "/etc/donkey/registry-ca/ca.crt" {
		t.Errorf("the mirror does not inherit the registry CA: %v", ca)
	}
	if strings.Count(got, `[registry."artifactory.example.com"]`) != 1 {
		t.Errorf("the mirror host is declared more than once:\n%s", got)
	}
}

// The config file's hash is what rolls buildkitd, so identical settings must
// render identically — a map iterated in Go's order would roll the build
// engine on every re-install.
func TestBuildkitTOMLIsStable(t *testing.T) {
	c := BuildkitConfig{
		RegistryHost:   "registry.example.com",
		RegistryCAFile: "/etc/donkey/registry-ca/ca.crt",
		Mirrors: map[string]string{
			"docker.io": "https://a.example.com/docker",
			"ghcr.io":   "https://b.example.com/ghcr",
			"gcr.io":    "https://c.example.com/gcr",
			"quay.io":   "https://d.example.com/quay",
		},
	}
	first := BuildkitTOML(c)
	for range 16 {
		if got := BuildkitTOML(c); got != first {
			t.Fatalf("the same settings rendered two different files:\n%s\n---\n%s", first, got)
		}
	}
}

// A mirror donkey talks to over plain http has to be declared as such, or
// BuildKit tries https and every pull fails.
func TestBuildkitTOMLPlainHTTPMirror(t *testing.T) {
	got := BuildkitTOML(BuildkitConfig{
		RegistryHost: "registry.example.com",
		Mirrors:      map[string]string{"docker.io": "http://mirror.internal:5000"},
	})
	parsed := parseBuildkitTOML(t, got)
	registry := parsed["registry"].(map[string]any)
	mirror, _ := registry["mirror.internal:5000"].(map[string]any)
	if mirror == nil {
		t.Fatalf("no section for the mirror host:\n%s", got)
	}
	if mirror["http"] != true {
		t.Errorf("the mirror is not marked plain http:\n%s", got)
	}
}

// A registry that is also its own mirror is one host, one table.
func TestBuildkitTOMLRegistryIsAlsoMirror(t *testing.T) {
	got := BuildkitTOML(BuildkitConfig{
		RegistryHost:   "artifactory.example.com",
		RegistryCAFile: "/etc/donkey/registry-ca/ca.crt",
		Mirrors:        map[string]string{"docker.io": "https://artifactory.example.com/docker-remote"},
	})
	parsed := parseBuildkitTOML(t, got)
	registry := parsed["registry"].(map[string]any)
	section, _ := registry["artifactory.example.com"].(map[string]any)
	if section == nil {
		t.Fatalf("no section for the shared host:\n%s", got)
	}
	if ca, _ := section["ca"].([]any); len(ca) != 1 {
		t.Errorf("the shared host lost its CA: %v", section)
	}
}

// Without mirrors, nothing about the file changes — this is what every
// existing installation renders.
func TestBuildkitTOMLWithoutMirrors(t *testing.T) {
	got := BuildkitTOML(BuildkitConfig{
		RegistryHost:   "registry.example.com",
		RegistryCAFile: "/etc/donkey/registry-ca/ca.crt",
	})
	want := "[worker.oci]\n  gc = true\n  reservedSpace = \"5GB\"\n  maxUsedSpace = \"8GB\"\n" +
		"\n[registry.\"registry.example.com\"]\n  ca = [\"/etc/donkey/registry-ca/ca.crt\"]\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMirrorHost(t *testing.T) {
	for mirror, want := range map[string]string{
		"https://jfrog.corp/docker-remote": "jfrog.corp/docker-remote",
		"jfrog.corp/docker-remote":         "jfrog.corp/docker-remote",
		"http://mirror.internal:5000":      "mirror.internal:5000",
		"  jfrog.corp/x/  ":                "jfrog.corp/x",
	} {
		if got, _ := mirrorHost(mirror); got != want {
			t.Errorf("mirrorHost(%q) = %q, want %q", mirror, got, want)
		}
	}
	if _, plain := mirrorHost("http://mirror.internal:5000"); !plain {
		t.Error("an http:// mirror was not recognized as plain http")
	}
	if _, plain := mirrorHost("https://jfrog.corp/x"); plain {
		t.Error("an https:// mirror was treated as plain http")
	}
}

// The build configuration has to land on both pods, in the shape each one
// reads it: donkey-server hands the build environment to every RUN, the build
// engine resolves images through the mirrors — and only the proxy is common
// to both.
func TestBuildConfigReachesBothPods(t *testing.T) {
	in := systemInput()
	in.Build = config.InstallBuild{
		BuildSettings: config.BuildSettings{
			Env:          map[string]string{"CGO_ENABLED": "0"},
			Instructions: "Use FROM registry.example.com/mirror/<image>:<tag>.",
		},
		RegistryMirrors: map[string]string{"docker.io": "https://mirror.example.com/docker"},
		Proxy:           config.InstallProxy{HTTPS: "http://proxy.example.com:3128", NoProxy: ".example.com"},
	}
	out, err := Render(SystemObjects(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// The build variables travel as JSON, the way DONKEY_ADMINS does.
		"DONKEY_BUILD_ENV",
		"CGO_ENABLED",
		"DONKEY_BUILDER_INSTRUCTIONS",
		"registry.example.com/mirror/<image>",
		// The mirrors are the build engine's business, and live in its config.
		`mirrors = ["mirror.example.com/docker"]`,
		// Both pods get the proxy, in both spellings.
		"HTTPS_PROXY",
		"https_proxy",
		"http://proxy.example.com:3128",
		"NO_PROXY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a configured build section should render %s", want)
		}
	}

	// Credentialed values never appear in a Deployment: build variables are
	// routinely "https://ci:token@jfrog.corp/...", and a Deployment is
	// readable by anyone who can read the namespace.
	for _, dep := range []*appsv1.Deployment{serverDeployment(in), buildkitDeployment(in)} {
		for _, c := range dep.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				switch e.Name {
				case "DONKEY_BUILD_ENV", "DONKEY_BUILDER_INSTRUCTIONS", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy":
					if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
						t.Errorf("%s/%s: %s is a plain value, not a secret reference", dep.Name, c.Name, e.Name)
					}
				}
			}
		}
	}

	// The build engine needs the proxy of its own, not just the server's.
	spec := buildkitPodSpec(in, true)
	var names []string
	for _, e := range spec.Containers[0].Env {
		names = append(names, e.Name)
	}
	if !slices.Contains(names, "HTTPS_PROXY") {
		t.Errorf("the build engine did not get the proxy: %v", names)
	}
}

// An installation that says nothing about builds renders what it always
// did — no empty DONKEY_BUILD_* variables, and a build engine with no
// environment at all.
func TestBuildConfigAbsentChangesNothing(t *testing.T) {
	in := systemInput()
	out, err := Render(SystemObjects(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "DONKEY_BUILD_") {
		t.Errorf("an unconfigured installation rendered build variables:\n%s", out)
	}
	if strings.Contains(out, "PROXY") {
		t.Error("an unconfigured installation rendered proxy variables")
	}
	if env := buildkitPodSpec(in, true).Containers[0].Env; len(env) != 0 {
		t.Errorf("the build engine got environment it didn't ask for: %v", env)
	}
}

// A forward proxy must never be applied to the addresses donkey's own
// components use to reach each other. Getting this wrong does not degrade
// anything — it takes the control plane off the air, because no corporate
// proxy can route a cluster address.
func TestProxyNeverAppliesInCluster(t *testing.T) {
	in := systemInput()
	in.Build.Proxy = config.InstallProxy{HTTPS: "http://proxy.example.com:3128"}
	spec := buildkitPodSpec(in, true)

	env := map[string]string{}
	for _, e := range spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	// An installation that named no exclusions of its own still gets these.
	for _, want := range []string{"localhost", "127.0.0.1", ".svc.cluster.local", ".cluster.local"} {
		if !strings.Contains(env["NO_PROXY"], want) {
			t.Errorf("NO_PROXY = %q, missing %s", env["NO_PROXY"], want)
		}
	}
	if !strings.Contains(env["no_proxy"], ".cluster.local") {
		t.Errorf("the lowercase spelling did not get the same exclusions: %q", env["no_proxy"])
	}

	// And whatever the installation configured survives alongside them.
	in.Build.Proxy.NoProxy = ".corp,10.0.0.0/8"
	env = map[string]string{}
	for _, e := range buildkitPodSpec(in, true).Containers[0].Env {
		env[e.Name] = e.Value
	}
	for _, want := range []string{".corp", "10.0.0.0/8", ".cluster.local"} {
		if !strings.Contains(env["NO_PROXY"], want) {
			t.Errorf("NO_PROXY = %q, missing %s", env["NO_PROXY"], want)
		}
	}

	// No proxy configured means no NO_PROXY invented for it.
	plain := systemInput()
	for _, e := range buildkitPodSpec(plain, true).Containers[0].Env {
		t.Errorf("an installation with no proxy got %s=%s", e.Name, e.Value)
	}
}

// A mirror key is a registry host and nothing else. Written as a URL — which
// is how a registry's own UI hands them out — it would render a section
// BuildKit never looks up, and every pull would silently go to the real
// registry: the one address a mirrored installation cannot reach.
func TestBuildkitTOMLNormalizesMirrorKeys(t *testing.T) {
	got := BuildkitTOML(BuildkitConfig{
		RegistryHost: "registry.example.com",
		Mirrors: map[string]string{
			"https://docker.io": "https://mirror.example.com/docker",
			"ghcr.io/":          "https://mirror.example.com/ghcr",
		},
	})
	parsed := parseBuildkitTOML(t, got)
	registry := parsed["registry"].(map[string]any)
	for _, host := range []string{"docker.io", "ghcr.io"} {
		if registry[host] == nil {
			t.Errorf("%s has no section — BuildKit would never match it:\n%s", host, got)
		}
	}
	if strings.Contains(got, "https://docker.io") {
		t.Errorf("a mirror key kept its scheme:\n%s", got)
	}
}

// The buildpacks builder makes the engine pull from donkey's own registry (a
// mirrored builder image lives there), and a pull carries no per-solve
// insecure attribute to fall back on.
func TestBuildkitTOMLInsecureRegistry(t *testing.T) {
	in := systemInput()
	in.RegistryInsecure = true
	parsed := parseBuildkitTOML(t, buildkitTOML(in))
	registry, _ := parsed["registry"].(map[string]any)
	if registry == nil {
		t.Fatalf("no registry section for an insecure registry:\n%s", buildkitTOML(in))
	}
	section, _ := registry["reg.example.com"].(map[string]any)
	if section == nil || section["http"] != true {
		t.Errorf("an insecure registry is not marked plain http: %v", registry)
	}
}

// donkey's own registry is routinely a bare service name that no domain
// suffix in NO_PROXY covers — so it is excluded by name.
func TestProxyExcludesRegistry(t *testing.T) {
	in := systemInput()
	in.RegistryURL = "donkey-registry:5000/donkey"
	in.Build.Proxy = config.InstallProxy{HTTPS: "http://proxy.example.com:3128"}
	for _, e := range buildkitPodSpec(in, true).Containers[0].Env {
		if e.Name == "NO_PROXY" && !strings.Contains(e.Value, "donkey-registry:5000") {
			t.Errorf("NO_PROXY = %q, missing donkey's own registry", e.Value)
		}
	}
}
