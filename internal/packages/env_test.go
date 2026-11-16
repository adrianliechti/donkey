package packages

import "testing"

func TestManagerEnv(t *testing.T) {
	for mirror, host := range map[string]string{
		"http://127.0.0.1:4874":                    "127.0.0.1",
		"http://donkey-packages.donkey-system.svc": "donkey-packages.donkey-system.svc",
		"http://donkey-packages:8090":              "donkey-packages",
	} {
		env := ManagerEnv(mirror)
		for name, want := range map[string]string{
			"NPM_CONFIG_REGISTRY": mirror + "/npm/",
			// Corepack joins the name of the package manager on with a slash
			// of its own.
			"COREPACK_NPM_REGISTRY":    mirror + "/npm",
			"YARN_NPM_REGISTRY_SERVER": mirror + "/npm/",
			"PIP_INDEX_URL":            mirror + "/pypi/",
			"UV_INDEX_URL":             mirror + "/pypi/",
			"GOPROXY":                  mirror + "/go/",
			"MAVEN_REPO_CENTRAL":       mirror + "/maven/",
			// The wrapper appends its path with a slash of its own.
			"MVNW_REPOURL": mirror + "/maven",
			// yarn takes hostnames here, without a port.
			"YARN_UNSAFE_HTTP_WHITELIST": host,
		} {
			if env[name] != want {
				t.Errorf("%s: %s = %q, want %q", mirror, name, env[name], want)
			}
		}
	}
}
