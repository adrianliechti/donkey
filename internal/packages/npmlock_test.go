package packages

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Compare the complete parsed documents, including fields npm may add in a
// newer version. Only the marked registry download fields should disappear.
func TestNPMLockfileFiltersRegistryDownloads(t *testing.T) {
	const public = "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"
	for _, tc := range []struct{ name, file, url, lock string }{
		{"public registry", "package-lock.json", public, `{
			"lockfileVersion": 3, "packages": {
				"": {"dependencies": {"left-pad": "^1"}},
				"node_modules/left-pad": {"version": "1.3.0", DOWNLOAD "integrity": "sha512-pinned"}
			}}`},
		{"nested npm 6 tree", "app/npm-shrinkwrap.json", public, `{
			"lockfileVersion": 1, "dependencies": {
				"left-pad": {"version": "1.3.0", DOWNLOAD "integrity": "sha512-top", "dependencies": {
					"left-pad": {"version": "1.2.0", DOWNLOAD "integrity": "sha512-nested"},
					"lib": {"version": "https://cdn.example/lib/-/lib.tgz", "integrity": "sha512-direct"}
				}}
			}}`},
		{"both npm 7 trees", "package-lock.json", public, `{
			"lockfileVersion": 2,
			"packages": {"node_modules/left-pad":{"version":"1.3.0", DOWNLOAD "integrity":"sha512-pin"}},
			"dependencies": {"left-pad":{"version":"1.3.0", DOWNLOAD "integrity":"sha512-pin"}}
		}`},
		{"workspace, alias and explicit tarballs", "npm-shrinkwrap.json", "http://old.example:4873/api/npm/@babel%2fcore/-/archived.tgz?sig=expired#sha1", `{
			"lockfileVersion": 3, "packages": {
				"": {"workspaces":["apps/*"], "devDependencies":{"alias":"npm:@babel/core@7.0.0"}},
				"apps/web": {"optionalDependencies":{"resolved":"https://cdn.example/pkg/-/explicit.tgz"}},
				"node_modules/alias": {"name":"@babel/core","version":"7.0.0", DOWNLOAD "integrity":"sha512-alias"},
				"node_modules/resolved": {"version":"1.0.0","resolved":"https://cdn.example/pkg/-/explicit.tgz"},
				"node_modules/web": {"resolved":"apps/web","link":true},
				"node_modules/git": {"version":"1.0.0","resolved":"git+ssh://git@host/lib.git#abc"},
				"node_modules/local": {"version":"1.0.0","resolved":"file:../lib.tgz"},
				"node_modules/github": {"version":"1.0.0","resolved":"https://npm.pkg.github.com/download/@acme/ui/1.0.0/abc"},
				"node_modules/gitlab": {"version":"1.0.0","resolved":"https://gitlab.example/api/v4/projects/7/packages/npm/@acme/ui/-/@acme/ui-1.0.0.tgz"}
			}, "extra":{"counter":9007199254740993,"resolved":"https://registry.npmjs.org/lib/-/lib.tgz"}
		}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.ReplaceAll(tc.lock, "DOWNLOAD", `"resolved":"`+tc.url+`",`)
			want := strings.ReplaceAll(tc.lock, "DOWNLOAD", "")
			got := RewriteLockfile(tc.file, []byte(input), "http://donkey-packages", nil)
			parse := func(s string) any {
				t.Helper()
				if !json.Valid([]byte(s)) {
					t.Fatalf("invalid JSON: %s", s)
				}
				var v any
				d := json.NewDecoder(strings.NewReader(s))
				d.UseNumber()
				if err := d.Decode(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if !reflect.DeepEqual(parse(string(got)), parse(want)) {
				t.Errorf("got %s\nwant %s", got, want)
			}
			if again := RewriteLockfile(tc.file, got, "http://another-mirror", nil); string(again) != string(got) {
				t.Errorf("filtering a second time changed the lockfile: %s", again)
			}
		})
	}
}

func TestNPMLockfilePassesThrough(t *testing.T) {
	for _, input := range []string{
		"{\r\n  \"packages\" : { \"node_modules/a\": {\"version\": \"1.0.0\", \"integrity\": \"sha512-a\"} }\r\n}\r\n",
		`{"packages":{"node_modules/a":{"version":"1.0.0","resolved":"https://acme.example/team/a/-/a.tgz"}}}`,
		`{"packages":{"node_modules/a":{"resolved":"https://npm.example/a/-/a.tgz"}}}`,
		`{"packages":{"node_modules/a":{"version":"1.0.0","resolved":"https://npm.example/a/-/a.tgz"}}`,
		`{"packages":{"node_modules/a":{"version":"1.0.0","resolved":"https://npm.example/a/-/a.tgz"}}} trailing`,
	} {
		got := RewriteLockfile("package-lock.json", []byte(input), "http://donkey-packages", []string{"https://acme.example/team/"})
		if string(got) != input {
			t.Errorf("changed %s to %s", input, got)
		}
	}
}
