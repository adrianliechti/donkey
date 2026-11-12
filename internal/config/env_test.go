package config

import "testing"

func TestServiceReferencesUseTheEnvMap(t *testing.T) {
	app := &AppConfig{Name: "api", Port: 8080, Addons: []AddonConfig{{Type: "postgres", Name: "db"}}, Env: map[string]string{"DB_URL": "${db.DATABASE_URL}", "MODE": "production"}}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		t.Fatal(err)
	}
	service, key, ok := ParseEnvRef(app.Env["DB_URL"])
	if !ok || service != "db" || key != "DATABASE_URL" {
		t.Fatal("reference was not parsed")
	}
	if _, _, ok := ParseEnvRef("prefix ${db.DATABASE_URL}"); ok {
		t.Fatal("partial substitutions are not service references")
	}
	app.Env["DB_URL"] = "${missing.DATABASE_URL}"
	if app.Validate() == nil {
		t.Fatal("undeclared service accepted")
	}
	app.Env["DB_URL"] = "${db.DATABASE_URL}"
	app.Build.Env = map[string]string{"DB_URL": app.Env["DB_URL"]}
	if app.Validate() == nil {
		t.Fatal("runtime credential accepted as build argument")
	}
}

func TestServiceReferencesCannotReachBrowserBuilds(t *testing.T) {
	for _, key := range []string{"VITE_DB", "NEXT_PUBLIC_DB", "PUBLIC_DB", "REACT_APP_DB", "NUXT_PUBLIC_DB", "DATABASE_URL"} {
		t.Run(key, func(t *testing.T) {
			app := &AppConfig{Name: "api", Port: 8080, Addons: []AddonConfig{{Type: "postgres", Name: "db"}}, Env: map[string]string{key: EnvRef("db", "DATABASE_URL")}}
			if key == "DATABASE_URL" {
				app.Build.Env = map[string]string{key: "postgres://localhost/development"}
			}
			app.ApplyDefaults()
			if err := app.Validate(); err == nil {
				t.Fatal("service binding could be exposed to a browser build")
			}
		})
	}
}
