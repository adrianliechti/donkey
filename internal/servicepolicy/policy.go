// Package servicepolicy evaluates the installation's service selection rules.
package servicepolicy

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

type Policy struct {
	query     rego.PreparedEvalQuery
	offerings map[string]config.OfferingPolicyMetadata
}

type Service struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Tags          []string `json:"tags"`
	Authorization string   `json:"authorization"`
	Protocol      string   `json:"protocol"`
	Category      string   `json:"category"`
}

func Compile(settings *config.ServicePolicy, proxies config.ProxyServices) (*Policy, error) {
	if settings == nil {
		return nil, nil
	}
	if settings.RegoFile != "" {
		return nil, fmt.Errorf("servicePolicy.regoFile must be resolved by the installer")
	}
	offerings := make(map[string]config.OfferingPolicyMetadata, len(settings.Offerings))
	for id, offering := range settings.Offerings {
		if !config.ValidName(id) || config.NormalizeAddonType(id) != id {
			return nil, fmt.Errorf("servicePolicy.offerings: %q must be a canonical catalog ID", id)
		}
		if err := offering.Validate(); err != nil {
			return nil, fmt.Errorf("servicePolicy.offerings.%s: %w", id, err)
		}
		offerings[id] = offering
	}
	for _, proxy := range proxies {
		meta := offerings[proxy.Name]
		if proxy.Authorization != "" {
			if meta.Authorization != "" && meta.Authorization != proxy.Authorization {
				return nil, fmt.Errorf("proxyServices.%s.authorization conflicts with servicePolicy.offerings", proxy.Name)
			}
			meta.Authorization = proxy.Authorization
		}
		if meta.Authorization == "delegated" {
			target, err := url.Parse(proxy.TargetURL)
			if err != nil || target.User != nil {
				return nil, fmt.Errorf("delegated proxy %s needs a target URL without shared credentials", proxy.Name)
			}
		}
		offerings[proxy.Name] = meta
	}
	mod, err := ast.ParseModule("servicePolicy.rego", settings.Rego)
	if err != nil {
		return nil, fmt.Errorf("servicePolicy.rego: %w", err)
	}
	if mod.Package.Path.String() != "data.donkey.services" || len(mod.RuleSet("deny")) == 0 {
		return nil, fmt.Errorf("servicePolicy.rego must define deny in package donkey.services")
	}
	query, err := rego.New(rego.Query("data.donkey.services.deny"), rego.ParsedModule(mod), rego.StrictBuiltinErrors(true)).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("servicePolicy.rego: %w", err)
	}
	return &Policy{query: query, offerings: offerings}, nil
}

func (p *Policy) Offering(desc *addonv1.Offering) Service {
	meta := p.offerings[desc.Id]
	tags := append([]string{}, desc.Tags...)
	tags = append(tags, meta.Tags...)
	slices.Sort(tags)
	auth := meta.Authorization
	if auth == "" {
		auth = "unknown"
	}
	return Service{ID: desc.Id, Tags: slices.Compact(tags), Authorization: auth, Protocol: desc.Protocol, Category: desc.Category}
}

// Decision returns a user-facing denial separately from a failed evaluation.
func (p *Policy) Decision(ctx context.Context, space string, services []Service) (string, error) {
	input := map[string]any{"space": space, "services": services}
	results, err := p.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return "", fmt.Errorf("evaluating service policy: %w", err)
	}
	if len(results) != 1 || len(results[0].Expressions) != 1 {
		return "", fmt.Errorf("service policy returned no decision")
	}
	denials, ok := results[0].Expressions[0].Value.([]any)
	if !ok {
		return "", fmt.Errorf("service policy deny must return a collection of messages")
	}
	messages := make([]string, 0, len(denials))
	for _, denial := range denials {
		msg, ok := denial.(string)
		if !ok || strings.TrimSpace(msg) == "" {
			return "", fmt.Errorf("service policy deny must contain nonempty messages")
		}
		messages = append(messages, msg)
	}
	slices.Sort(messages)
	return strings.Join(messages, "\n"), nil
}
