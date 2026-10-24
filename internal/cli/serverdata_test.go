package cli

import (
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestDockerBundleKeepsDataInTheStateVolume(t *testing.T) {
	render := func(data config.InstallData) composeFile {
		t.Helper()
		settings := &config.InstallFile{
			Runtime: "docker", Image: "ghcr.io/example/donkey-server:test", Domain: "apps.localhost",
			TLS:  config.InstallTLS{Mode: "ca"},
			Auth: config.InstallAuth{Issuer: "https://id.example.com", ClientID: "donkey"},
			AI:   config.InstallAI{BaseURL: "https://api.openai.com/v1", Model: "gpt-6-sol"},
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
	byDefault := render(config.InstallData{})
	if got := byDefault.Services["orchestrator"].Environment["DONKEY_DATA_DIR"]; got != dockerDataDir {
		t.Fatalf("DONKEY_DATA_DIR = %q, want the state volume %s", got, dockerDataDir)
	}
	// The orchestrator owns the history; the API forwards every read to it.
	if got := byDefault.Services["server"].Environment["DONKEY_DATA_DIR"]; got != "" {
		t.Fatalf("donkey-server should have no data dir, got %q", got)
	}
}
