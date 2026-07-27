package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     GlobalConfig
		wantErr bool
	}{
		{"empty is fine (not logged in)", GlobalConfig{}, false},
		{"valid server", GlobalConfig{Server: ServerConfig{URL: "https://donkey.example.com", Token: "t"}}, false},
		{"bad scheme", GlobalConfig{Server: ServerConfig{URL: "donkey.example.com"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestServerTokenPrecedence(t *testing.T) {
	s := ServerConfig{Token: "from-file"}
	t.Setenv("DONKEY_TOKEN", "from-env")
	if s.ResolveToken() != "from-env" {
		t.Fatal("env token should win")
	}
	t.Setenv("DONKEY_TOKEN", "")
	if s.ResolveToken() != "from-file" {
		t.Fatal("file token should be used")
	}
}

func TestRegistryPasswordPrecedence(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Registry{PasswordFile: f}

	t.Setenv("DONKEY_REGISTRY_PASSWORD", "from-env")
	if p, _ := r.Password(); p != "from-env" {
		t.Fatalf("env should win, got %q", p)
	}

	t.Setenv("DONKEY_REGISTRY_PASSWORD", "")
	if p, _ := r.Password(); p != "from-file" {
		t.Fatalf("file should be used (trimmed), got %q", p)
	}
}

func TestLoadGlobalMissingIsNil(t *testing.T) {
	c, err := LoadGlobal(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || c != nil {
		t.Fatalf("missing file should be (nil, nil), got (%v, %v)", c, err)
	}
}

func TestGlobalSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.yaml")
	in := &GlobalConfig{
		Server:      ServerConfig{URL: "http://donkey.example.com", Token: "dk_x"},
		KubeContext: "kind",
	}
	if err := in.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file should be 0600, got %v", fi.Mode().Perm())
	}
	out, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Server.URL != in.Server.URL || out.KubeContext != "kind" {
		t.Fatalf("roundtrip lost data: %+v", out)
	}
}

func TestAppValidate(t *testing.T) {
	valid := func() AppConfig {
		c := AppConfig{Name: "my-app", Port: 8000}
		c.ApplyDefaults()
		return c
	}
	cases := []struct {
		name    string
		mutate  func(*AppConfig)
		wantErr bool
	}{
		{"valid", func(c *AppConfig) {}, false},
		{"uppercase name", func(c *AppConfig) { c.Name = "MyApp" }, true},
		{"empty name", func(c *AppConfig) { c.Name = "" }, true},
		{"bad port", func(c *AppConfig) { c.Port = 0 }, true},
		{"bad build type", func(c *AppConfig) { c.Build.Type = "nixpacks" }, true},
		{"ai build type", func(c *AppConfig) { c.Build.Type = BuildAI }, false},
		{"bad health path", func(c *AppConfig) { c.Healthcheck.Path = "healthz" }, true},
		{"dup addon names", func(c *AppConfig) {
			c.Addons = []AddonConfig{{Name: "db", Type: "postgres"}, {Name: "db", Type: "redis"}}
		}, true},
		// `host` becomes a route hostname, a certificate name and part of the
		// login callback URL — anything but one of its three legal shapes
		// breaks one of them, and a wildcard would hand the app everything
		// under it.
		{"host: full hostname", func(c *AppConfig) { c.Host = "shop.example.com" }, false},
		{"host: space root", func(c *AppConfig) { c.Host = "@" }, false},
		{"host: own label", func(c *AppConfig) { c.Host = "shop" }, false},
		{"host: wildcard", func(c *AppConfig) { c.Host = "*.example.com" }, true},
		{"host: with scheme", func(c *AppConfig) { c.Host = "https://shop.example.com" }, true},
		{"host: with path", func(c *AppConfig) { c.Host = "shop.example.com/app" }, true},
		{"host: with port", func(c *AppConfig) { c.Host = "shop.example.com:8443" }, true},
		{"host: uppercase", func(c *AppConfig) { c.Host = "Shop.example.com" }, true},
		{"host: trailing dot", func(c *AppConfig) { c.Host = "shop.example.com." }, true},
		{"host: leading dash", func(c *AppConfig) { c.Host = "-shop.example.com" }, true},
		{"host: uppercase label", func(c *AppConfig) { c.Host = "Shop" }, true},
		{"host: on internal app", func(c *AppConfig) { c.Internal, c.Host = true, "@" }, true},
		// Access refines the login gateway — it cannot exist where there is
		// no login (public) or no address to guard (internal, no routes).
		{"access", func(c *AppConfig) { c.Access = &AccessConfig{Users: []string{"ada@example.com"}} }, false},
		{"access roles", func(c *AppConfig) { c.Access = &AccessConfig{Roles: []string{"developer"}} }, false},
		{"access on public app", func(c *AppConfig) {
			c.Public = true
			c.Access = &AccessConfig{Groups: []string{"staff"}}
		}, true},
		{"access on internal app", func(c *AppConfig) {
			c.Internal = true
			c.Access = &AccessConfig{Groups: []string{"staff"}}
		}, true},
		{"access on internal app with routes", func(c *AppConfig) {
			c.Internal = true
			c.Routes = []RouteConfig{{App: "web", Path: "/api"}}
			c.Access = &AccessConfig{Groups: []string{"staff"}}
		}, false},
		{"access entry with spaces", func(c *AppConfig) {
			c.Access = &AccessConfig{Users: []string{"ada lovelace"}}
		}, true},
		{"empty access is fine", func(c *AppConfig) { c.Access = &AccessConfig{} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			if err := c.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestAppDefaults(t *testing.T) {
	c := AppConfig{Name: "x", Port: 80, Addons: []AddonConfig{{Type: "postgres"}}}
	c.ApplyDefaults()
	if c.Build.Type != BuildAuto {
		t.Fatalf("build type default not applied: %+v", c.Build)
	}
	if c.Build.Platform != "" {
		t.Fatalf("platform should stay empty (the server owns the default), got %q", c.Build.Platform)
	}
	if *c.Replicas != 1 {
		t.Fatalf("replica default not applied")
	}
	if c.Healthcheck.Path != "" {
		t.Fatalf("healthcheck should default to empty (TCP probe), got %q", c.Healthcheck.Path)
	}
	if c.Addons[0].Name != "postgres" {
		t.Fatalf("addon name should default to its type, got %q", c.Addons[0].Name)
	}
}

func TestResolve(t *testing.T) {
	global := &GlobalConfig{Server: ServerConfig{URL: "http://d.example.com"}, KubeContext: "prod"}
	r := Resolve(Flags{}, global, nil)
	if r.KubeContext != "prod" || r.ServerURL() != "http://d.example.com" {
		t.Fatalf("global values should apply: %+v", r)
	}
	r = Resolve(Flags{KubeContext: "staging"}, global, nil)
	if r.KubeContext != "staging" {
		t.Fatalf("flag context should win, got %q", r.KubeContext)
	}
}

func TestStorageValidate(t *testing.T) {
	withStorage := func(s *StorageConfig) AppConfig {
		c := AppConfig{Name: "my-app", Port: 8000, Storage: s}
		c.ApplyDefaults()
		return c
	}
	cases := []struct {
		name    string
		storage *StorageConfig
		wantErr bool
	}{
		{"no storage", nil, false},
		{"empty list is a decision", &StorageConfig{Paths: []string{}}, false},
		{"absolute paths", &StorageConfig{Paths: []string{"/app/data", "/var/lib/app"}}, false},
		{"relative path", &StorageConfig{Paths: []string{"data"}}, true},
		{"root", &StorageConfig{Paths: []string{"/"}}, true},
		{"colliding folders", &StorageConfig{Paths: []string{"/app/data", "/app-data"}}, true},
		{"bad size", &StorageConfig{Size: "big", Paths: []string{"/app/data"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withStorage(tc.storage)
			if err := c.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestStorageDefaultsAndReplicas(t *testing.T) {
	c := AppConfig{Name: "x", Port: 80, Storage: &StorageConfig{Paths: []string{"/app/data"}}}
	c.ApplyDefaults()
	if c.Storage.Size != DefaultStorage {
		t.Fatalf("storage size default not applied, got %q", c.Storage.Size)
	}
	if !c.HasStorage() {
		t.Fatal("an app with paths has storage")
	}
	// A disk attaches to one machine at a time — say so instead of failing later.
	two := int32(2)
	c.Replicas = &two
	if err := c.Validate(); err == nil {
		t.Fatal("storage with several copies should be refused")
	}
}

func TestStorageSubPathIsStable(t *testing.T) {
	cases := map[string]string{
		"/app/data":        "app-data",
		"/app/data/":       "app-data",
		"/var/lib/uploads": "var-lib-uploads",
		"/data":            "data",
		"/App/Data":        "app-data",
	}
	for in, want := range cases {
		if got := StorageSubPath(in); got != want {
			t.Errorf("StorageSubPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveSpace(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		app      *AppConfig
		appDir   string
		repoRoot string
		want     string
	}{
		{"flag wins", "Prod Team", &AppConfig{Space: "from-file"}, "/x/web", "/x", "prod-team"},
		{"donkey.yaml next", "", &AppConfig{Space: "from-file"}, "/x/web", "/x", "from-file"},
		{"repository name groups a monorepo", "", nil, "/home/me/acme/web", "/home/me/acme", "acme"},
		{"single-app repo", "", nil, "/home/me/blog", "/home/me/blog", "blog"},
		{"no repository: the folder itself", "", nil, "/home/me/scratch", "", "scratch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveSpace(tc.flag, tc.app, tc.appDir, tc.repoRoot); got != tc.want {
				t.Fatalf("ResolveSpace() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppSaveKeepsSpaceAndStorage(t *testing.T) {
	dir := t.TempDir()
	c := AppConfig{Name: "web", Space: "acme", Port: 3000,
		Storage: &StorageConfig{Paths: []string{"/app/uploads"}}}
	c.ApplyDefaults()
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	out, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Space != "acme" || len(out.Storage.Paths) != 1 || out.Storage.Paths[0] != "/app/uploads" {
		t.Fatalf("roundtrip lost space or storage: %+v", out)
	}
}

// Save minimizes an addon whose name just repeats its type — that must drop
// the field entirely, not leave a `name: ""` behind.
func TestAppSaveOmitsDefaultAddonName(t *testing.T) {
	dir := t.TempDir()
	c := AppConfig{Name: "web", Port: 3000, Addons: []AddonConfig{{Type: "openai"}}}
	c.ApplyDefaults()
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, AppFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "name: \"\"") {
		t.Fatalf("minimized addon leaked an empty name:\n%s", b)
	}
	out, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Addons) != 1 || out.Addons[0].Name != "openai" {
		t.Fatalf("roundtrip lost the addon or its defaulted name: %+v", out.Addons)
	}
}

// The shipped example must actually load — it is the documentation for the
// install file's shape.
func TestExampleInstallFileParses(t *testing.T) {
	f, err := LoadInstallFile(filepath.Join("..", "..", "examples", "server-install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Domain == "" || f.Registry.URL == "" {
		t.Fatalf("example lost its key settings: %+v", f)
	}
}
