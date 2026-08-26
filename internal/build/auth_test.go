package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestRegistryAuthUsesPrefixHostForPushesAndSameHostMirrors(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "s3cret")
	reg := config.Registry{
		URL:      "registry.example.com/platform/donkey",
		Username: "builder",
	}
	got, err := registryAuthConfig(reg, "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "builder" || got.Password != "s3cret" {
		t.Fatalf("the prefixed registry lost its push credentials: %+v", got)
	}
	got, err = registryAuthConfig(reg, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "" || got.Password != "" {
		t.Fatalf("enterprise credentials leaked to a public registry: %+v", got)
	}
}

func TestRegistryAuthUsesDockerConfigForSeparateMirrorHost(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "push-secret")
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.json")
	contents := `{"auths":{"mirror.example.com":{"username":"mirror-user","password":"mirror-secret"}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{
		URL:             "push.example.com/platform/donkey",
		Username:        "push-user",
		DockerConfigDir: dir,
	}
	got, err := registryAuthConfig(reg, "mirror.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "mirror-user" || got.Password != "mirror-secret" {
		t.Fatalf("mirror credentials were not selected by host: %+v", got)
	}

	rotated := `{"auths":{"mirror.example.com":{"username":"mirror-user","password":"rotated-secret"}}}`
	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = registryAuthConfig(reg, "mirror.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "rotated-secret" {
		t.Fatalf("rotated mirror credentials were not reloaded: %+v", got)
	}
}
