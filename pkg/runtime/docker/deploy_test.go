package docker

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestImagePullOptionsUseRegistryCredentialsOnlyForItsHost(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "s3cret")
	r := &Runtime{opts: Options{Registry: config.Registry{
		URL: "registry.example.com/team/donkey", Username: "builder",
	}}}

	opts, err := r.imagePullOptions("registry.example.com/mirror/library/postgres:16")
	if err != nil {
		t.Fatal(err)
	}
	if opts.RegistryAuth == "" {
		t.Fatal("an image on the enterprise registry should receive its login")
	}
	raw, err := base64.URLEncoding.DecodeString(opts.RegistryAuth)
	if err != nil {
		t.Fatal(err)
	}
	var auth map[string]string
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatal(err)
	}
	if auth["username"] != "builder" || auth["password"] != "s3cret" || auth["serveraddress"] != "registry.example.com" {
		t.Fatalf("wrong pull auth: %v", auth)
	}

	public, err := r.imagePullOptions("docker.io/library/postgres:16")
	if err != nil {
		t.Fatal(err)
	}
	if public.RegistryAuth != "" {
		t.Fatal("registry credentials must not be sent to a public registry")
	}
}

func TestPullRefOnlyRewritesForBundledRegistry(t *testing.T) {
	const imageRef = "registry.example.com/platform/donkey/shop:build"
	external := &Runtime{opts: Options{Registry: config.Registry{URL: "registry.example.com/platform/donkey"}}}
	if got := external.pullRef(imageRef); got != imageRef {
		t.Fatalf("external registry ref was rewritten: %q", got)
	}

	bundled := &Runtime{opts: Options{
		Registry:     config.Registry{URL: "donkey-registry:5000/donkey"},
		PullRegistry: "127.0.0.1:5000",
	}}
	if got, want := bundled.pullRef("donkey-registry:5000/donkey/shop:build"), "127.0.0.1:5000/donkey/shop:build"; got != want {
		t.Fatalf("bundled registry ref = %q, want %q", got, want)
	}
}
