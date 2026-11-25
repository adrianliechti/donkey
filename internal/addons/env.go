package addons

import (
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// BindEnvironment uses the keys a service actually supplies. External
// services may supply only an endpoint, leaving credentials in the app's env.
func BindEnvironment(app *config.AppConfig, service string, values map[string]string) error {
	return bindEnvironment(app, service, values, nil)
}

// PlanEnvironment validates explicit references against the offering contract.
// Optional keys can be injected once provisioning actually supplies them.
func PlanEnvironment(app *config.AppConfig, service string, desc *addonv1.Offering) error {
	optional := map[string]bool{}
	for _, v := range desc.Env {
		optional[v.Name] = v.Optional
	}
	return bindEnvironment(app, service, EnvTemplate(desc), optional)
}

func bindEnvironment(app *config.AppConfig, service string, values map[string]string, optional map[string]bool) error {
	for env, raw := range app.Env {
		if name, key, ref := config.ParseEnvRef(raw); ref && name == service {
			if _, exists := values[key]; !exists {
				return fmt.Errorf("env.%s: service %s does not provide %s", env, service, key)
			}
		}
	}
	if app.Env == nil {
		app.Env = map[string]string{}
	}
	for key := range values {
		if _, _, ref := config.ParseEnvRef(app.Env[key]); !ref && !optional[key] {
			app.Env[key] = config.EnvRef(service, key)
		}
	}
	return nil
}
