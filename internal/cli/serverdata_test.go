package cli

import (
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestDockerBundleKeepsServerDataInTheStateVolume(t *testing.T) {
	render := func(data config.InstallData) composeFile {
		t.Helper()
		settings := &config.InstallFile{
			Runtime: "docker", Image: "ghcr.io/example/donkey-server:test", Domain: "apps.localhost",
			TLS:  config.InstallTLS{Mode: "ca"},
			Auth: config.InstallAuth{Issuer: "https://id.example.com", ClientID: "donkey"},
			AI:   config.InstallAI{BaseURL: "https://api.openai.com/v1", Model: "gpt-5.6-sol"},
			Data: data,
		}
		files, err := renderDockerBundle(settings)
		if err != nil {
			t.Fatal(err)
		}
		var compose composeFile
		if err := yaml.Unmarshal(files["compose.yaml"].Data, &compose); err != nil {
			t.Fatal(err)
		}
		return compose
	}
	if got := render(config.InstallData{}).Services["server"].Environment["DONKEY_DATA_DIR"]; got != dockerDataDir {
		t.Fatalf("DONKEY_DATA_DIR = %q, want the state volume %s", got, dockerDataDir)
	}
	if got := render(config.InstallData{Disabled: true}).Services["server"].Environment["DONKEY_DATA_DIR"]; got != "" {
		t.Fatalf("a disabled data volume leaves the server without one, got %q", got)
	}
}
