package build

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

// The lifecycle pushes the image itself, so its report is the only place the
// digest of what was actually pushed exists.
func TestReadCNBReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.toml")
	const report = `[image]
  tags = ["registry.example.com/donkey/shop:abc-20260727120000"]
  digest = "sha256:5f3c0d5b2a1e4c9d8b7a6f5e4d3c2b1a0987654321fedcba9876543210abcdef"
  image-id = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  manifest-size = 1234
`
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readCNBReport(path)
	if err != nil {
		t.Fatalf("readCNBReport: %v", err)
	}
	const want = "sha256:5f3c0d5b2a1e4c9d8b7a6f5e4d3c2b1a0987654321fedcba9876543210abcdef"
	if got != want {
		t.Errorf("digest = %q, want %q", got, want)
	}
}

func TestReadCNBReportFailures(t *testing.T) {
	dir := t.TempDir()
	// No report at all: the lifecycle didn't get far enough to write one.
	if _, err := readCNBReport(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("a missing report was not reported")
	}
	// A report with no digest is not an answer — donkey must not hand back an
	// empty one and let a deploy proceed as if it knew what it pushed.
	path := filepath.Join(dir, "report.toml")
	if err := os.WriteFile(path, []byte("[image]\n  tags = [\"x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCNBReport(path); err == nil {
		t.Error("a report with no digest was accepted")
	}
}

// Both spellings of the run image are in the wild; a builder that names one
// must not need configuration to be usable.
func TestBuilderRunImage(t *testing.T) {
	for name, tc := range map[string]struct {
		metadata string
		want     string
	}{
		"stack": {
			`{"stack":{"runImage":{"image":"paketobuildpacks/run-jammy-base","mirrors":[]}}}`,
			"paketobuildpacks/run-jammy-base",
		},
		"runImages": {
			`{"runImages":[{"image":"gcr.io/buildpacks/google-22/run"}]}`,
			"gcr.io/buildpacks/google-22/run",
		},
		"both agree": {
			`{"runImages":[{"image":"a/run"}],"stack":{"runImage":{"image":"a/run"}}}`,
			"a/run",
		},
		"absent":  {`{}`, ""},
		"garbage": {`not json`, ""},
		"empty":   {``, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := builderRunImage(tc.metadata); got != tc.want {
				t.Errorf("builderRunImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Raw LLB inherits nothing from the image it starts from, so what donkey
// reads out of the builder's configuration is what the lifecycle gets.
func TestParseBuilderImage(t *testing.T) {
	cfg := `{"config":{
		"Env":["PATH=/cnb/lifecycle:/usr/bin","CNB_USER_ID=1001","CNB_GROUP_ID=1002","CNB_PLATFORM_API=0.13"],
		"User":"cnb",
		"Labels":{"io.buildpacks.builder.metadata":"{\"stack\":{\"runImage\":{\"image\":\"example/run\"}}}"}
	}}`
	got, err := parseBuilderImage([]byte(cfg))
	if err != nil {
		t.Fatalf("parseBuilderImage: %v", err)
	}
	if got.UID != 1001 || got.GID != 1002 {
		t.Errorf("uid/gid = %d/%d, want 1001/1002", got.UID, got.GID)
	}
	if got.RunImage != "example/run" {
		t.Errorf("run image = %q", got.RunImage)
	}
	if len(got.Env) != 4 {
		t.Errorf("the builder's environment was not carried over: %v", got.Env)
	}
}

// A builder that says nothing about its user still has to build — the CNB
// convention is 1000.
func TestParseBuilderImageDefaults(t *testing.T) {
	got, err := parseBuilderImage([]byte(`{"config":{}}`))
	if err != nil {
		t.Fatalf("parseBuilderImage: %v", err)
	}
	if got.UID != cnbDefaultUID || got.GID != cnbDefaultGID {
		t.Errorf("uid/gid = %d/%d, want %d/%d", got.UID, got.GID, cnbDefaultUID, cnbDefaultGID)
	}
	if _, err := parseBuilderImage([]byte("not json")); err == nil {
		t.Error("an unreadable image configuration was accepted")
	}
}

func TestCNBRegistryAuth(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "s3cret")
	b := &BuildKitBuilder{Registry: config.Registry{
		URL:      "registry.example.com/donkey",
		Username: "ci",
	}}
	raw, err := b.cnbRegistryAuth(nil)
	if err != nil {
		t.Fatalf("cnbRegistryAuth: %v", err)
	}
	var auth map[string]string
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatalf("the lifecycle would not read this: %v (%s)", err, raw)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:s3cret"))
	if auth["registry.example.com"] != want {
		t.Errorf("auth = %v, want %s for registry.example.com", auth, want)
	}

	// A mirror that needs credentials donkey has no other reason to know is
	// configured through the build environment, and wins outright.
	raw, err = b.cnbRegistryAuth(map[string]string{cnbRegistryAuthEnv: `{"mirror.example.com":"Basic zzz"}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"mirror.example.com":"Basic zzz"}` {
		t.Errorf("a configured CNB_REGISTRY_AUTH was not used: %s", raw)
	}
}

// The cache has to be keyed to the app. Image tags carry a timestamp, so a
// cache keyed on the full reference would be empty on every deploy.
func TestCNBCacheID(t *testing.T) {
	first := cnbCacheID("registry.example.com/donkey/shop:abc123-20260727120000")
	second := cnbCacheID("registry.example.com/donkey/shop:def456-20260727130000")
	if first != second {
		t.Errorf("two deploys of one app got different caches: %q vs %q", first, second)
	}
	if other := cnbCacheID("registry.example.com/donkey/blog:abc123-20260727120000"); other == first {
		t.Errorf("two different apps share a cache: %q", other)
	}
	// A registry's port colon is not a tag.
	if got := cnbCacheID("registry.example.com:5000/donkey/shop"); !strings.HasSuffix(got, "registry.example.com:5000/donkey/shop") {
		t.Errorf("a registry port was mistaken for a tag: %q", got)
	}
}

func TestRegistryHostOf(t *testing.T) {
	for ref, want := range map[string]string{
		"registry.example.com/donkey/shop:v1": "registry.example.com",
		"donkey-registry:5000/donkey/shop:v1": "donkey-registry:5000",
	} {
		if got := registryHostOf(ref); got != want {
			t.Errorf("registryHostOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The lifecycle refuses to start without being told a platform version, and
// there is no safe constant: too new and an old builder rejects it, too old
// and a new one has dropped support. The builder says what it takes.
func TestBuilderPlatformAPI(t *testing.T) {
	for name, tc := range map[string]struct {
		metadata string
		want     string
	}{
		// The newest supported wins — and "0.9" must not beat "0.15", which
		// is what comparing these as strings would do.
		"supported list": {
			`{"lifecycle":{"api":{"platform":"0.7"},"apis":{"platform":{"supported":["0.7","0.9","0.15","0.12"]}}}}`,
			"0.15",
		},
		// Older builders list only what they were built against.
		"single api":   {`{"lifecycle":{"api":{"platform":"0.8"}}}`, "0.8"},
		"no lifecycle": {`{"stack":{}}`, ""},
		"garbage":      {`not json`, ""},
		"empty":        {``, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := builderPlatformAPI(tc.metadata); got != tc.want {
				t.Errorf("builderPlatformAPI() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareAPIVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int // sign
	}{
		{"0.15", "0.9", +1},
		{"0.9", "0.15", -1},
		{"0.12", "0.12", 0},
		{"1.0", "0.99", +1},
		{"0.7", "", +1},
		{"", "0.7", -1},
		{"", "", 0},
	} {
		got := compareAPIVersions(tc.a, tc.b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("compareAPIVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// A builder that names its platform API must be usable with no configuration
// at all — that is what makes `builder: buildpacks` a one-line setting.
func TestParseBuilderImageReadsPlatformAPI(t *testing.T) {
	cfg := `{"config":{"Labels":{"io.buildpacks.builder.metadata":
		"{\"lifecycle\":{\"apis\":{\"platform\":{\"supported\":[\"0.12\",\"0.14\"]}}},\"stack\":{\"runImage\":{\"image\":\"example/run\"}}}"}}}`
	got, err := parseBuilderImage([]byte(cfg))
	if err != nil {
		t.Fatalf("parseBuilderImage: %v", err)
	}
	if got.PlatformAPI != "0.14" {
		t.Errorf("platform API = %q, want 0.14", got.PlatformAPI)
	}
}
