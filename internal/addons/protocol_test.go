package addons

import (
	"context"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestPluginProtocolInheritanceAndMismatch(t *testing.T) {
	response := sampleProvisioning()
	server := &testPluginServer{typ: "protocol-plugin", protocol: "mysql", provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return response, nil
	}}
	closePlugins, err := ConnectPlugins(context.Background(), config.AddonPlugins{{Type: server.typ, Address: startPlugin(t, server), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer closePlugins()
	a, _ := Get(server.typ)
	for _, protocol := range []string{"", "mysql", "mariadb"} {
		response.Protocol = protocol
		got, err := a.(*Plugin).Provision(context.Background(), &addonv1.ProvisionRequest{})
		if err != nil || got.Protocol != "mysql" {
			t.Fatalf("protocol %q: %v", protocol, err)
		}
	}
	for _, protocol := range []string{"postgres", "mysql://password@db"} {
		response.Protocol = protocol
		if _, err := a.(*Plugin).Provision(context.Background(), &addonv1.ProvisionRequest{}); err == nil {
			t.Fatalf("accepted protocol %q", protocol)
		}
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
	if err := ValidateProtocol("custom-database"); err != nil {
		t.Fatal(err)
	}
}
