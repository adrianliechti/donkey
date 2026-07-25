package config

import (
	"os"
	"path/filepath"
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
		{"bad build type", func(c *AppConfig) { c.Build.Type = "buildpacks" }, true},
		{"bad health path", func(c *AppConfig) { c.Healthcheck.Path = "healthz" }, true},
		{"dup addon names", func(c *AppConfig) {
			c.Addons = []AddonConfig{{Name: "db", Type: "postgres"}, {Name: "db", Type: "redis"}}
		}, true},
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
