package build

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The build receives lockfiles that name the mirror, with a size that
// matches — BuildKit transfers by the walked size — while the app's files on
// disk, and everything that is not a lockfile, stay as uploaded.
func TestMirroredContextRewritesLockfilesForTheBuildOnly(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"yarn.lock":                       `resolved "https://registry.yarnpkg.com/is-odd/-/is-odd-3.0.1.tgz#65101baf"`,
		"services/api/uv.lock":            `source = { registry = "http://127.0.0.1:4874/pypi/" }`,
		"services/api/main.py":            `INDEX = "https://pypi.org/simple"`,
		"pnpm-lock.yaml":                  "lockfileVersion: '9.0'\n",
		"node_modules/left-pad/yarn.lock": `resolved "https://registry.yarnpkg.com/x/-/x-1.0.0.tgz"`,
		// A registry the app assigns to a scope is kept; any other host is not.
		"web/.npmrc":            "@acme:registry=https://npm.acme.example/\n",
		"web/package-lock.json": `"resolved": "https://npm.acme.example/@acme/ui/-/ui-1.0.0.tgz", "resolved": "https://artifactory.example.com/api/npm/npm/is-odd/-/is-odd-3.0.1.tgz"`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mirrored, err := mirroredContextFS(dir, "http://donkey-packages:8090")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"yarn.lock":                       `resolved "http://donkey-packages:8090/npm/is-odd/-/is-odd-3.0.1.tgz#65101baf"`,
		"services/api/uv.lock":            `source = { registry = "http://donkey-packages:8090/pypi/" }`,
		"services/api/main.py":            files["services/api/main.py"],
		"pnpm-lock.yaml":                  files["pnpm-lock.yaml"],
		"node_modules/left-pad/yarn.lock": files["node_modules/left-pad/yarn.lock"],
		"web/.npmrc":                      files["web/.npmrc"],
		"web/package-lock.json":           `"resolved": "https://npm.acme.example/@acme/ui/-/ui-1.0.0.tgz", "resolved": "http://donkey-packages:8090/npm/is-odd/-/is-odd-3.0.1.tgz"`,
	}
	sizes := map[string]int64{}
	if err := mirrored.Walk(context.Background(), "", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			sizes[filepath.ToSlash(path)] = info.Size()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, content := range want {
		reader, err := mirrored.Open(filepath.FromSlash(name))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(reader)
		reader.Close()
		if string(got) != content {
			t.Errorf("%s reads %s, want %s", name, got, content)
		}
		if sizes[name] != int64(len(content)) {
			t.Errorf("%s walked with size %d, contents are %d bytes", name, sizes[name], len(content))
		}
		onDisk, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if string(onDisk) != files[name] {
			t.Errorf("%s was changed on disk", name)
		}
	}
}

// The mirror address a build is given is the one its lockfiles are pointed
// at — also where the installation names the mirror by IP and port.
func TestSolveContextUsesTheBuildersMirrorAddress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`source = { registry = "https://pypi.org/simple" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	builder := BuildKitBuilder{PackagesURL: "http://172.31.250.10:8090/"}
	mirrored, err := mirroredContextFS(dir, builder.packagesURL())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := mirrored.Open("uv.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, _ := io.ReadAll(reader)
	index := builder.buildEnv(nil)["UV_INDEX_URL"]
	if !strings.Contains(string(got), `registry = "`+index+`"`) {
		t.Errorf("uv.lock names %s, the build's UV_INDEX_URL is %s", got, index)
	}
}
