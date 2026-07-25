package doctor

import (
	"context"

	"monobox.ch/donkey/internal/config"
)

// Check IDs used by commands to declare preflight requirements.
const (
	CheckConnection = "connection"
	CheckAppConfig  = "app-config"
)

// ConnectionCheck verifies the donkey server answers and accepts the token.
func ConnectionCheck(ping func(ctx context.Context) error, serverURL string) *Check {
	return &Check{
		ID:      CheckConnection,
		Name:    "Connected to the donkey server",
		Explain: "All builds and deployments run on the donkey server.",
		Fix:     "Run `donkey login` with the server address and token you were given.",
		Run: func(ctx context.Context, env *Env) Result {
			if serverURL == "" {
				return Result{Status: Fail, Detail: "this machine isn't connected to a donkey server yet."}
			}
			if err := ping(ctx); err != nil {
				return Result{Status: Fail, Detail: "the server at " + serverURL + " didn't accept the connection.", Err: err}
			}
			return Result{Status: OK, Detail: serverURL}
		},
	}
}

// AppConfigCheck validates donkey.yaml when present in the app directory.
func AppConfigCheck() *Check {
	return &Check{
		ID:      CheckAppConfig,
		Name:    "App settings (donkey.yaml)",
		Explain: "donkey.yaml describes the app in this folder.",
		Fix:     "Fix the reported problem in donkey.yaml, or delete the file and run `donkey init` again.",
		Run: func(ctx context.Context, env *Env) Result {
			app, err := config.LoadApp(env.AppDir)
			if err != nil {
				return Result{Status: Fail, Detail: "this folder's donkey.yaml has a problem.", Err: err}
			}
			if app == nil {
				return Result{Status: Info, Detail: "no app in this folder yet (run `donkey init` inside a project)."}
			}
			env.App = app
			return Result{Status: OK, Detail: app.Name}
		},
	}
}
