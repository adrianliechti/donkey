package addons

import (
	"context"
	"strings"
	"testing"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestPluginProtocolGovernsTheConnectionURL(t *testing.T) {
	response := sampleProvisioning()
	server := &testPluginServer{typ: "protocol-plugin", protocol: "mariadb", provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return response, nil
	}}
	p := connectTestPlugin(t, server)
	desc, err := p.Describe(context.Background())
	if err != nil || NormalizeProtocol(desc.Protocol) != ProtocolMySQL {
		t.Fatalf("mariadb should normalize to mysql: %+v %v", desc, err)
	}
	if _, err := p.Provision(context.Background(), &addonv1.ProvisionRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	response.Env["DATABASE_URL"] = "postgres://donkey:secret@db.addons.svc/app"
	if _, err := p.Provision(context.Background(), &addonv1.ProvisionRequest{}, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("accepted a URL of another protocol (or leaked it): %v", err)
	}
}

func TestProtocolConnectionContracts(t *testing.T) {
	for _, tc := range []struct {
		protocol, key, url string
		valid              bool
	}{
		{"postgresql", "DATABASE_URL", "postgresql://user:secret@db/app", true},
		{"mariadb", "DATABASE_URL", "mysql://user:secret@db/app", true},
		{"redis", "REDIS_URL", "rediss://user:secret@cache:6379/0", true},
		{"s3", "AWS_ENDPOINT_URL_S3", "https://s3.example.test", true},
		{"openai", "OPENAI_BASE_URL", "https://ai.example.test/api/v1", true},
		{"custom", "CUSTOM_URL", "provider-specific", true},
		{"mysql", "DATABASE_URL", "postgres://user:secret@db/app", false},
		{"redis", "REDIS_URL", "http://user:secret@cache", false},
		{"s3", "AWS_ENDPOINT_URL_S3", "https://user:secret@store", false},
		{"s3", "AWS_ENDPOINT_URL_S3", "https://store/bucket", false},
		{"openai", "OPENAI_BASE_URL", "https://ai/v1?key=secret", false},
		{"openai", "OPENAI_BASE_URL", "file:///secret", false},
		{"postgres", "DATABASE_URL", "", false},
	} {
		err := ValidateBinding(tc.protocol, map[string]string{tc.key: tc.url})
		if (err == nil) != tc.valid {
			t.Errorf("%s validation: %v", tc.protocol, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("connection error disclosed credentials")
		}
	}
	info := &addonv1.DescribeResponse{Type: "vendor", Title: "Vendor", Protocol: "redis", Env: []*addonv1.EnvVar{{Name: "CUSTOM_URL"}}}
	if validateDescription("vendor", info) == nil {
		t.Fatal("advertised a console without its connection key")
	}
	info.Protocol = "custom"
	if err := validateDescription("vendor", info); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProtocol("custom-database"); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitProtocolSurvivesLegacyLookingEnvironment(t *testing.T) {
	metadata := (Metadata{Type: "vendor", Protocol: "custom"}).Resolve(map[string]string{"DATABASE_URL": "postgres://db/app"})
	if metadata.Protocol != "custom" || metadata.Type != "vendor" {
		t.Fatal("explicit provider metadata was inferred from env")
	}
}

func TestLegacyProtocolUsesURLScheme(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"postgresql://host/db", "postgres"}, {"mysql://host/db", "mysql"},
		{"mariadb://host/db", "mysql"}, {"unknown://host/db", ""}, {"not a url", ""},
	} {
		if got := LegacyProtocol(map[string]string{"DATABASE_URL": tc.url}); got != tc.want {
			t.Fatalf("%q: %q", tc.url, got)
		}
	}
}
