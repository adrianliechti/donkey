package addons

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

// openai is a binding-only addon: nothing runs in the space. Apps get the
// platform's AI through donkey-server's proxy, and the binding's key is a
// per-space stand-in the proxy checks and swaps — the real credential never
// leaves donkey-system.
type openai struct{}

func (openai) Type() string { return "openai" }
func (openai) Description() string {
	return "The platform's AI, OpenAI-compatible — no key of your own to manage."
}
func (openai) CredentialKey() string  { return "OPENAI_API_KEY" }
func (openai) DefaultVersion() string { return "" }
func (openai) DefaultStorage() string { return "" }

func (openai) Binding(in Input) map[string]string {
	b := map[string]string{
		// The runtime provider says where donkey-server's AI proxy is
		// reachable from apps — its address is runtime vocabulary.
		"OPENAI_BASE_URL": in.AIProxyURL,
		"OPENAI_API_KEY":  in.Password,
	}
	// Binding runs inside donkey-server, which knows the configured model —
	// telling the app saves it from guessing one the upstream doesn't have.
	model := os.Getenv("DONKEY_AI_MODEL")
	if model == "" {
		model = config.DefaultAIModel
	}
	b["OPENAI_MODEL"] = model
	return b
}

// ExternalBinding points apps at an OpenAI-compatible endpoint that already
// exists — a real OpenAI account, a company gateway. Keys for those belong
// in the app's own env, not in a URL.
func (openai) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("that doesn't look like an OpenAI-compatible address — expected https://api.openai.com/v1")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the address names no host — expected https://api.openai.com/v1")
	}
	return map[string]string{
		"OPENAI_BASE_URL": strings.TrimRight(raw, "/"),
	}, nil
}

// Workload is nil on purpose — the service is the platform itself.
func (openai) Workload(in Input) (*Workload, error) { return nil, nil }
