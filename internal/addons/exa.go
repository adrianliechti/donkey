package addons

import (
	"context"
	"fmt"
	"strings"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// exa binds apps to the platform's search endpoint with a per-space token.
// The real upstream credential stays in donkey-ai.
type exa struct{}

func (exa) Type() string          { return "exa" }
func (exa) CredentialKey() string { return "EXA_API_KEY" }

func (e exa) Describe(context.Context) (*addonv1.Offering, error) {
	desc := description(e, "Exa", "Web search and page contents through the platform's Exa-compatible endpoint.",
		CategoryAI, ProtocolExa, ObjectSchema(nil), "EXA_API_KEY")
	desc.Capabilities = nil
	return desc, nil
}

func (exa) Binding(in Input) map[string]string {
	return map[string]string{"EXA_BASE_URL": in.ExaProxyURL, "EXA_API_KEY": in.Password}
}

// Attached endpoints supply only the URL; an app provides its own key.
func (exa) ExternalBinding(raw string) (map[string]string, error) {
	binding := map[string]string{"EXA_BASE_URL": strings.TrimRight(raw, "/")}
	if err := ValidateBinding(ProtocolExa, binding); err != nil {
		return nil, fmt.Errorf("expected an Exa-compatible http or https address without credentials, query or fragment")
	}
	return binding, nil
}

func (exa) Workload(Input) (*Workload, error) { return nil, nil }
