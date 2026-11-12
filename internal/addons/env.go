package addons

import (
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
)

// BindEnvironment uses the keys a service actually supplies. External
// services may supply only an endpoint, leaving credentials in the app's env.
func BindEnvironment(app *config.AppConfig, service string, values map[string]string) error {
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
		if _, _, ref := config.ParseEnvRef(app.Env[key]); !ref {
			app.Env[key] = config.EnvRef(service, key)
		}
	}
	return nil
}
