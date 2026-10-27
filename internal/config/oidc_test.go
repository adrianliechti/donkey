package config

import (
	"strings"
	"testing"
)

func TestOIDCAppRegistrationValidation(t *testing.T) {
	for _, tc := range []struct {
		uri   string
		valid bool
	}{
		{"https://web.example/callback", true},
		{"https://web.example/callback?instance=1", true},
		{"http://localhost:3000/callback", true},
		{"http://127.0.0.1:3000/callback", true},
		{"http://[::1]:3000/callback", true},
		{"http://web.example/callback", false},
		{"https://*.example/callback", false},
		{"https://web.example/callback#", false},
		{"https://user@web.example/callback", false},
		{"https://web.example/oauth2/callback", false},
		{"https://web.example/%6fauth2/callback", false},
		{"//web.example/callback", false},
		{"https://:443/callback", false},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			c := &OIDCClient{RedirectURIs: []string{tc.uri}}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("validation = %v", err)
			}
		})
	}
	for _, hash := range []string{"", strings.Repeat("a", 64)} {
		if err := (&OIDCClient{RedirectURIs: []string{"https://web.example/cb"}, ClientSecretSHA256: hash}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if (&OIDCClient{RedirectURIs: []string{"https://web.example/cb"}, ClientSecretSHA256: "secret"}).Validate() == nil {
		t.Fatal("raw secret accepted")
	}
	if (&OIDCClient{}).Validate() == nil {
		t.Fatal("empty callback list accepted")
	}
	app := &AppConfig{Name: "web", Port: 8080, OIDC: &OIDCClient{RedirectURIs: []string{"https://web.example/cb"}}}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		t.Fatal(err)
	}
	app.Internal = true
	if app.Validate() == nil {
		t.Fatal("internal OIDC app accepted")
	}
	app.Internal, app.Mode = false, AppModeAPI
	if app.Validate() == nil {
		t.Fatal("API OIDC app accepted")
	}
}
