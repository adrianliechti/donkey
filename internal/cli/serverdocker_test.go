package cli

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestRegistryPullAuthUsesEnterpriseHostWithPrefixedURL(t *testing.T) {
	reg := config.Registry{URL: "registry.example.com/platform/donkey", Username: "builder"}
	encoded, err := registryPullAuth(reg, "builder", "s3cret", "registry.example.com/mirror/moby/buildkit:v1")
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("same-host mirrored image should receive registry auth")
	}
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var auth map[string]string
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatal(err)
	}
	if auth["username"] != "builder" || auth["password"] != "s3cret" || auth["serveraddress"] != "registry.example.com" {
		t.Fatalf("wrong registry auth: %v", auth)
	}

	public, err := registryPullAuth(reg, "builder", "s3cret", "docker.io/moby/buildkit:v1")
	if err != nil {
		t.Fatal(err)
	}
	if public != "" {
		t.Fatal("enterprise credentials must not be sent to a public registry")
	}
}

func TestBuilderEnvMapIncludesInstallationInstructions(t *testing.T) {
	got := builderEnvMap(config.BuildSettings{Instructions: "Use approved base-image prefixes."})
	if got["DONKEY_BUILDER_INSTRUCTIONS"] != "Use approved base-image prefixes." {
		t.Fatalf("builder instructions missing from Docker server env: %v", got)
	}
}
