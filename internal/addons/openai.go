package addons

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// openai is a binding-only addon: nothing runs in the space. Apps get the
// platform's AI through donkey-server's proxy, and the binding's key is a
// per-space stand-in the proxy checks and swaps — the real credential never
// leaves donkey-system.
type openai struct{}

func (openai) Type() string          { return "openai" }
func (openai) CredentialKey() string { return "OPENAI_API_KEY" }

func (o openai) Describe(context.Context) (*addonv1.Offering, error) {
	desc := description(o, "OpenAI", "The platform's AI, OpenAI-compatible — no key of your own to manage.",
		CategoryAI, ProtocolOpenAI, ObjectSchema(nil), "OPENAI_API_KEY")
	// Nothing runs and nothing is stored, so there is no data to keep, and
	// there are no parameters to update.
	desc.Capabilities = nil
	return desc, nil
}

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
	binding := map[string]string{"OPENAI_BASE_URL": strings.TrimRight(raw, "/")}
	if err := ValidateBinding(ProtocolOpenAI, binding); err != nil {
		return nil, fmt.Errorf("expected an OpenAI-compatible http or https address without credentials, query or fragment")
	}
	return binding, nil
}

// Workload is nil on purpose — the service is the platform itself.
func (openai) Workload(in Input) (*Workload, error) { return nil, nil }
