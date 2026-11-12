package packages

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pythonhosted = "aHR0cHM6Ly9maWxlcy5weXRob25ob3N0ZWQub3Jn" // https://files.pythonhosted.org

// Whatever host a lockfile names for a registry download — the public
// registry under either name, an enterprise one, a third one nobody told
// donkey about, `donkey packages` on a developer's machine — the build asks
// the mirror for the same package and file. Versions and checksums are not
// touched.
func TestRewriteLockfilePointsRegistryAddressesAtTheMirror(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"package-lock.json": {
			`"resolved": "https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz", "integrity": "sha512-CQpn"` + "\n" +
				`"resolved": "https://artifactory.example.com/api/npm/npm/@types/node/-/node-22.1.0.tgz", "integrity": "sha512-Wu1V"`,
			`"resolved": "http://donkey-packages/npm/is-odd/-/is-odd-3.0.1.tgz", "integrity": "sha512-CQpn"` + "\n" +
				`"resolved": "http://donkey-packages/npm/@types/node/-/node-22.1.0.tgz", "integrity": "sha512-Wu1V"`,
		},
		"yarn.lock": {
			`  resolved "https://registry.yarnpkg.com/is-odd/-/is-odd-3.0.1.tgz#65101baf"` + "\n" +
				`  resolved "http://127.0.0.1:4874/npm/is-number/-/is-number-6.0.0.tgz#e6d15ad3"` + "\n" +
				`  resolved "https://nexus.other.example/repository/npm-all/left-pad/-/left-pad-1.3.0.tgz#5b8a3a7"`,
			`  resolved "http://donkey-packages/npm/is-odd/-/is-odd-3.0.1.tgz#65101baf"` + "\n" +
				`  resolved "http://donkey-packages/npm/is-number/-/is-number-6.0.0.tgz#e6d15ad3"` + "\n" +
				`  resolved "http://donkey-packages/npm/left-pad/-/left-pad-1.3.0.tgz#5b8a3a7"`,
		},
		"apps/web/bun.lock": {
			`"is-odd": ["is-odd@3.0.1", "http://localhost:51234/npm/is-odd/-/is-odd-3.0.1.tgz", {}, "sha512-CQpn"],`,
			`"is-odd": ["is-odd@3.0.1", "http://donkey-packages/npm/is-odd/-/is-odd-3.0.1.tgz", {}, "sha512-CQpn"],`,
		},
		"pnpm-lock.yaml": {
			`resolution: {integrity: sha512-CQpn, tarball: https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz}`,
			`resolution: {integrity: sha512-CQpn, tarball: http://donkey-packages/npm/is-odd/-/is-odd-3.0.1.tgz}`,
		},
		// A registry that writes a scoped name as one path segment.
		"npm-shrinkwrap.json": {
			`"resolved": "http://verdaccio.example:4873/@babel%2fcore/-/core-7.0.0.tgz"`,
			`"resolved": "http://donkey-packages/npm/@babel/core/-/core-7.0.0.tgz"`,
		},
		// uv repeats the index in a dependency entry wherever the lock holds
		// two versions of a package; a lock that named the mirror on the
		// package and the old index there would not even parse. Line endings
		// are whatever the checkout made them.
		"api/uv.lock": {
			"[[package]]\r\nname = \"anyio\"\r\n" + `source = { registry = "https://pypi.org/simple" }` + "\r\ndependencies = [\r\n" +
				`    { name = "idna", version = "3.6", source = { registry = "https://pypi.org/simple" }, marker = "python_full_version < '3.12'" },` + "\r\n]\r\n" +
				`wheels = [{ url = "https://files.pythonhosted.org/packages/7b/a2/anyio-4.4.0-py3-none-any.whl" }]` + "\r\n\r\n" +
				"[[package]]\r\nname = \"fx\"\r\n" + `source = { virtual = "." }` + "\r\ndependencies = [\r\n" +
				`    { name = "idna", version = "3.6", source = { registry = "https://pypi.org/simple" }, marker = "python_full_version < '3.12'" },` + "\r\n]\r\n",
			"[[package]]\r\nname = \"anyio\"\r\n" + `source = { registry = "http://donkey-packages/pypi/" }` + "\r\ndependencies = [\r\n" +
				`    { name = "idna", version = "3.6", source = { registry = "http://donkey-packages/pypi/" }, marker = "python_full_version < '3.12'" },` + "\r\n]\r\n" +
				`wheels = [{ url = "http://donkey-packages/pypi/-/resource/` + pythonhosted + `/packages/7b/a2/anyio-4.4.0-py3-none-any.whl" }]` + "\r\n\r\n" +
				"[[package]]\r\nname = \"fx\"\r\n" + `source = { virtual = "." }` + "\r\ndependencies = [\r\n" +
				`    { name = "idna", version = "3.6", source = { registry = "http://donkey-packages/pypi/" }, marker = "python_full_version < '3.12'" },` + "\r\n]\r\n",
		},
		// The public index keeps its exact path on the mirror.
		"uv.lock": {
			"[[package]]\nname = \"idna\"\n" +
				`source = { registry = "https://pypi.org/simple" }` + "\n" +
				`sdist = { url = "https://files.pythonhosted.org/packages/21/ed/idna-3.7.tar.gz", hash = "sha256:028f" }`,
			"[[package]]\nname = \"idna\"\n" +
				`source = { registry = "http://donkey-packages/pypi/" }` + "\n" +
				`sdist = { url = "http://donkey-packages/pypi/-/resource/` + pythonhosted + `/packages/21/ed/idna-3.7.tar.gz", hash = "sha256:028f" }`,
		},
		// So does `donkey packages`, which is this mirror.
		"services/api/uv.lock": {
			"[[package]]\n" + `source = { registry = "http://127.0.0.1:4874/pypi/" }` + "\n" +
				`wheels = [{ url = "http://127.0.0.1:4874/pypi/-/resource/` + pythonhosted + `/packages/e5/3e/idna-3.7-py3-none-any.whl" }]`,
			"[[package]]\n" + `source = { registry = "http://donkey-packages/pypi/" }` + "\n" +
				`wheels = [{ url = "http://donkey-packages/pypi/-/resource/` + pythonhosted + `/packages/e5/3e/idna-3.7-py3-none-any.whl" }]`,
		},
		// Any other index is asked for the file by its name.
		"worker/uv.lock": {
			"[[package]]\n" + `source = { registry = "https://artifactory.example.com/api/pypi/pypi/simple" }` + "\n" +
				"wheels = [\n" +
				`    { url = "https://artifactory.example.com/api/pypi/pypi/packages/packages/e5/3e/idna-3.7-py3-none-any.whl", hash = "sha256:82fe" },` + "\n" +
				`    { url = "https://files.example.org/torch-2.5.1%2Bcpu-cp312-cp312-linux_x86_64.whl?sig=1#frag", hash = "sha256:7f2a" },` + "\n]",
			"[[package]]\n" + `source = { registry = "http://donkey-packages/pypi/" }` + "\n" +
				"wheels = [\n" +
				`    { url = "http://donkey-packages/pypi/-/file/idna-3.7-py3-none-any.whl", hash = "sha256:82fe" },` + "\n" +
				`    { url = "http://donkey-packages/pypi/-/file/torch-2.5.1%2Bcpu-cp312-cp312-linux_x86_64.whl", hash = "sha256:7f2a" },` + "\n]",
		},
	} {
		if !IsLockfile(name) {
			t.Errorf("%s is not taken for a lockfile", name)
		}
		if got := string(RewriteLockfile(name, []byte(tc.in), "http://donkey-packages/", nil)); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, tc.want)
		}
	}
}

// What is not a registry download stays where it points: a dependency from
// git, from a plain URL or a path, a registry whose downloads have another
// shape, and a lockfile of a kind that records no addresses.
func TestRewriteLockfileLeavesOtherAddressesAlone(t *testing.T) {
	for name, content := range map[string]string{
		"package-lock.json": `"resolved": "https://npm.pkg.github.com/download/@acme/ui/1.0.0/abc", "resolved": "git+ssh://git@github.com/acme/lib.git#1f2e"` +
			`, "resolved": "https://gitlab.example.com/api/v4/projects/7/packages/npm/@acme/ui/-/@acme/ui-1.0.0.tgz"`,
		"yarn.lock": `resolved "https://codeload.github.com/acme/lib/tar.gz/1f2e"` + "\n" + `resolved "https://example.com/downloads/lib-1.0.0.tgz"`,
		"uv.lock": "[[package]]\n" + `source = { url = "https://example.com/pkg-1.0.tar.gz" }` + "\n" +
			`sdist = { url = "https://example.com/pkg-1.0.tar.gz", hash = "sha256:aa" }` + "\n" +
			"[[package]]\n" + `source = { git = "https://github.com/acme/lib?rev=1f2e#1f2e" }` + "\n" +
			"[[package]]\n" + `source = { virtual = "." }` + "\n\n[package.metadata]\n" +
			`requires-dist = [{ name = "pkg", url = "https://example.com/pkg-1.0.tar.gz" }]`,
		// A dependency someone wrote as a URL is compared with package.json:
		// its name in the lockfile has to stay what package.json says.
		"web/yarn.lock":  `"is-odd@https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz":` + "\n" + `  version "3.0.1"`,
		"pnpm-lock.yaml": `specifier: https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz` + "\n" + `'is-odd@https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz':`,
		"bun.lock":       `"dependencies": { "is-odd": "https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz" }` + "\n" + `"is-odd": ["is-odd@https://registry.npmjs.org/is-odd/-/is-odd-3.0.1.tgz", {}],`,
		"go.sum":         `rsc.io/quote v1.5.2 h1:w5fcysjrx7yqtD/aO+QwRjYZOKnaM9Uh2b40tElTs3Y= "resolved": "https://registry.npmjs.org/x/-/x-1.0.0.tgz"`,
		"poetry.lock":    `url = "https://files.pythonhosted.org/packages/21/ed/idna-3.7.tar.gz"`,
	} {
		if got := string(RewriteLockfile(name, []byte(content), "http://donkey-packages", nil)); got != content {
			t.Errorf("%s was changed to %s", name, got)
		}
	}
	if IsLockfile("go.sum") || IsLockfile("poetry.lock") {
		t.Error("a file that is not translated is taken for a lockfile")
	}
}

// A registry the app itself assigns to particular packages is not what the
// build's registry setting replaces, so its addresses stay — while the rest
// of the same lockfile goes to the mirror.
func TestRewriteLockfileKeepsRegistriesTheAppDeclares(t *testing.T) {
	declared := DeclaredRegistries(".npmrc", []byte(
		"registry=https://artifactory.example.com/api/npm/npm/\n"+
			"@acme:registry=https://npm.acme.example/team/\n"+
			"//npm.acme.example/team/:_authToken=${ACME_TOKEN}\n"))
	declared = append(declared, DeclaredRegistries(".yarnrc", []byte(`"@legacy:registry" "https://npm.legacy.example"`+"\n"))...)
	declared = append(declared, DeclaredRegistries("pyproject.toml", []byte(
		"[project]\nname = \"app\"\n\n"+
			"[[tool.uv.index]]\nname = \"pytorch\"\nurl = \"https://download.pytorch.org/whl/cpu\"\nexplicit = true\n\n"+
			"[[tool.uv.index]]\nurl = \"https://artifactory.example.com/api/pypi/pypi/simple\"\ndefault = true\n\n"+
			"[tool.uv.sources]\ntorch = { index = \"pytorch\" }\n"))...)
	declared = append(declared, DeclaredRegistries("uv.toml", []byte("[[index]]\nurl = \"https://wheels.acme.example/simple\"\n"))...)
	declared = append(declared, DeclaredRegistries("bunfig.toml", []byte(
		"[install]\nregistry = \"https://artifactory.example.com/api/npm/npm/\"\n\n[install.scopes]\n"+
			"\"@bun\" = \"https://npm.bun.example/\"\n"+
			"\"@tok\" = { token = \"$TOKEN\", url = \"https://npm.tok.example/\" }\n"))...)
	want := []string{"https://npm.acme.example/team/", "https://npm.legacy.example", "https://download.pytorch.org/whl/cpu", "https://wheels.acme.example/simple",
		"https://npm.bun.example/", "https://npm.tok.example/"}
	if strings.Join(declared, " ") != strings.Join(want, " ") {
		t.Fatalf("declared registries = %q, want %q — the default registry is the one a build replaces", declared, want)
	}

	npm := `"resolved": "https://npm.acme.example/team/@acme/ui/-/ui-1.0.0.tgz"` + "\n" +
		`"resolved": "https://npm.acme.example/team-other/@acme/ui/-/ui-1.0.0.tgz"` + "\n" +
		`"resolved": "https://artifactory.example.com/api/npm/npm/is-odd/-/is-odd-3.0.1.tgz"`
	wantNPM := `"resolved": "https://npm.acme.example/team/@acme/ui/-/ui-1.0.0.tgz"` + "\n" +
		`"resolved": "http://donkey-packages/npm/@acme/ui/-/ui-1.0.0.tgz"` + "\n" +
		`"resolved": "http://donkey-packages/npm/is-odd/-/is-odd-3.0.1.tgz"`
	if got := string(RewriteLockfile("package-lock.json", []byte(npm), "http://donkey-packages", declared)); got != wantNPM {
		t.Errorf("package-lock.json:\n got %s\nwant %s", got, wantNPM)
	}

	uv := "[[package]]\nname = \"torch\"\n" + `source = { registry = "https://download.pytorch.org/whl/cpu" }` + "\n" +
		`wheels = [{ url = "https://download.pytorch.org/whl/cpu/torch-2.5.1%2Bcpu-cp312-cp312-linux_x86_64.whl", hash = "sha256:7f2a" }]` + "\n\n" +
		"[[package]]\nname = \"idna\"\n" + `source = { registry = "https://artifactory.example.com/api/pypi/pypi/simple" }` + "\n" +
		`wheels = [{ url = "https://artifactory.example.com/api/pypi/pypi/packages/packages/e5/3e/idna-3.7-py3-none-any.whl", hash = "sha256:82fe" }]`
	wantUV := "[[package]]\nname = \"torch\"\n" + `source = { registry = "https://download.pytorch.org/whl/cpu" }` + "\n" +
		`wheels = [{ url = "https://download.pytorch.org/whl/cpu/torch-2.5.1%2Bcpu-cp312-cp312-linux_x86_64.whl", hash = "sha256:7f2a" }]` + "\n\n" +
		"[[package]]\nname = \"idna\"\n" + `source = { registry = "http://donkey-packages/pypi/" }` + "\n" +
		`wheels = [{ url = "http://donkey-packages/pypi/-/file/idna-3.7-py3-none-any.whl", hash = "sha256:82fe" }]`
	if got := string(RewriteLockfile("uv.lock", []byte(uv), "http://donkey-packages", declared)); got != wantUV {
		t.Errorf("uv.lock:\n got %s\nwant %s", got, wantUV)
	}
}

// A lockfile from the open internet names files.pythonhosted.org, and one
// from anywhere else is translated to ask for its files by name. An
// enterprise index has the file under a path of its own, so the mirror looks
// it up in that index's page for the project — with the upstream's
// credential, and for that host alone.
func TestHandlerFindsPublicPyPIFilesAtAnEnterpriseUpstream(t *testing.T) {
	const upstreamURL = "https://pypi.mirror.example.com/api/pypi/pypi/simple"
	var requests []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("reader:token"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want the upstream's credential", got)
		}
		status, body := http.StatusOK, "wheel"
		switch r.URL.Path {
		case "/api/pypi/pypi/simple/python-dateutil/":
			body = `<a href="../../packages/packages/aa/bb/python_dateutil-2.9.0-py2.py3-none-any.whl?sig=abc#sha256=1f2e">w</a>` +
				`<a href="../../packages/packages/cc/dd/python-dateutil-2.9.0.tar.gz#sha256=3a4b">s</a>`
		case "/api/pypi/pypi/simple/absent/":
			status, body = http.StatusNotFound, ""
		}
		return &http.Response{
			StatusCode: status, Header: http.Header{"Content-Type": []string{"text/html"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})
	proxy, err := handler(Config{
		NPMUpstream: "https://registry.npmjs.org", PyPIUpstream: upstreamURL,
		GoUpstream: "https://proxy.golang.org", MavenUpstream: "https://repo.maven.apache.org/maven2",
		NuGetUpstream: "https://api.nuget.org/v3/index.json",
	}, func(host string) (string, string, bool, error) {
		return "reader", "token", host == "pypi.mirror.example.com", nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}
	get := func(file string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://donkey-packages/pypi/-/resource/"+pythonhosted+"/packages/21/ed/"+file, nil))
		return rec
	}

	if rec := get("python-dateutil-2.9.0.tar.gz"); rec.Code != http.StatusOK || rec.Body.String() != "wheel" {
		t.Fatalf("sdist: %d %s", rec.Code, rec.Body)
	}
	// A lockfile that named any other host asks by the file's name alone.
	named := httptest.NewRecorder()
	proxy.ServeHTTP(named, httptest.NewRequest(http.MethodGet, "http://donkey-packages/pypi/-/file/python-dateutil-2.9.0.tar.gz", nil))
	if named.Code != http.StatusOK || named.Body.String() != "wheel" {
		t.Fatalf("by name: %d %s", named.Code, named.Body)
	}
	requests = requests[:2]
	if rec := get("python_dateutil-2.9.0-py2.py3-none-any.whl"); rec.Code != http.StatusOK {
		t.Fatalf("wheel: %d %s", rec.Code, rec.Body)
	}
	want := []string{
		upstreamURL + "/python-dateutil/",
		"https://pypi.mirror.example.com/api/pypi/pypi/packages/packages/cc/dd/python-dateutil-2.9.0.tar.gz",
		upstreamURL + "/python-dateutil/",
		// An index that signs its links has them asked for as it wrote them.
		"https://pypi.mirror.example.com/api/pypi/pypi/packages/packages/aa/bb/python_dateutil-2.9.0-py2.py3-none-any.whl?sig=abc",
	}
	if got := strings.Join(requests, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("upstream requests:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}

	// A file the index does not list, a project it does not have, and a host
	// that is neither the upstream nor the public file host are refused as
	// before.
	for _, file := range []string{"python_dateutil-9.9.9-py3-none-any.whl", "absent-1.0.tar.gz", "notes.txt"} {
		if rec := get(file); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", file, rec.Code)
		}
	}
	other := base64.RawURLEncoding.EncodeToString([]byte("https://files.example.org"))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://donkey-packages/pypi/-/resource/"+other+"/python-dateutil-2.9.0.tar.gz", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("another host: status %d, want 403", rec.Code)
	}
}

func TestPyPIProject(t *testing.T) {
	for file, want := range map[string]string{
		"idna-3.7-py3-none-any.whl":                  "idna",
		"python_dateutil-2.9.0-py2.py3-none-any.whl": "python-dateutil",
		"python-dateutil-2.8.2.tar.gz":               "python-dateutil",
		"ruamel.yaml-0.18.6.tar.gz":                  "ruamel-yaml",
		"Flask_SQLAlchemy-3.1.1-py3-none-any.whl":    "flask-sqlalchemy",
		"zope.interface-6.0.zip":                     "zope-interface",
		"README.txt":                                 "",
		"-1.0.tar.gz":                                "",
	} {
		if got := pypiProject(file); got != want {
			t.Errorf("pypiProject(%q) = %q, want %q", file, got, want)
		}
	}
}
