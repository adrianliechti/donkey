package docker

import (
	"context"
	"github.com/docker/docker/api/types/network"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"

	"github.com/adrianliechti/donkey/internal/addons"
)

func TestServiceBindingsSeparateSameNamedServicesAcrossSpaces(t *testing.T) {
	transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		space := "one"
		if strings.Contains(req.URL.Path, "donkey.two.postgres") {
			space = "two"
		}
		labels := addonLabels(space, "postgres", roleAddon)
		labels[labelPort] = "5432"
		return dockerJSONResponse(http.StatusOK, map[string]any{"Id": space, "Config": map[string]any{"Labels": labels}}), nil
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
	for _, space := range []string{"one", "two"} {
		// Simulate pre-upgrade bindings: same alias, different credentials.
		binding := map[string]string{"DATABASE_URL": "postgres://donkey:" + space + "@postgres:5432/app", "PGHOST": "postgres", "PGPASSWORD": space}
		if err := r.state.setBinding(space, sharedOwner, "postgres", binding); err != nil {
			t.Fatal(err)
		}
		got, err := r.ServiceBinding(context.Background(), space, sharedOwner, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(got["DATABASE_URL"])
		password, _ := u.User.Password()
		if u.Host != "donkey."+space+".postgres:5432" || password != space || got["PGHOST"] != "donkey."+space+".postgres" {
			t.Fatalf("binding escaped its space or changed credentials: %+v", got)
		}
	}
}

func TestServiceMetadataAndAIProxyCredentials(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{AIContainer: "donkey-ai"}}
	ctx := context.Background()
	for _, fixture := range []struct{ name, endpoint, key, owner string }{
		{"platform", r.aiProxyURL(), "platform-token", sharedOwner},
		{"vendor", "https://vendor.example/v1", "vendor-secret", sharedOwner},
		{"copy", r.aiProxyURL(), "app-token", "api"},
	} {
		if err := r.state.setBinding("team", fixture.owner, fixture.name, map[string]string{"OPENAI_BASE_URL": fixture.endpoint, "OPENAI_API_KEY": fixture.key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.state.writeJSON(addons.Metadata{Type: "external-model", Protocol: "custom"}, "service-metadata", "team", "vendor.json"); err != nil {
		t.Fatal(err)
	}
	connection, err := r.ServiceConnection(ctx, "team", "vendor")
	if err != nil || connection.Type != "external-model" || connection.Protocol != "custom" {
		t.Fatalf("provider metadata: %+v %v", connection, err)
	}
	tokens, err := r.AIProxyTokens(ctx)
	if err != nil || !reflect.DeepEqual(tokens, []string{"platform-token"}) {
		t.Fatalf("wrong proxy tokens: %v", err)
	}
	for _, folder := range []string{"env", "bindings", "provisioning"} {
		if err := r.state.writeJSON(map[string]string{"secret": "private"}, folder, "team", "private.json"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(r.state.path(folder, "team", "private.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s credentials are not private: %v", folder, err)
		}
	}
}

func TestBindingHostRewritePreservesCredentialsAndPaths(t *testing.T) {
	for _, kind := range []string{"postgres", "redis", "s3"} {
		t.Run(kind, func(t *testing.T) {
			entry, _ := addons.Get(context.Background(), kind)
			addon := entry.(addons.Builtin)
			host := kind + ".donkey-one.svc"
			original := addon.Binding(addons.Input{Name: kind, Namespace: "donkey-one", Password: host})
			got := rewriteBindingHost(original, host, ctrName("one", kind))
			if got[addon.CredentialKey()] != host {
				t.Fatal("a password that resembles a hostname was rewritten")
			}
			for key, raw := range got {
				if !strings.Contains(raw, "://") {
					continue
				}
				u, err := url.Parse(strings.TrimPrefix(raw, "jdbc:"))
				if err != nil {
					t.Fatal(err)
				}
				if u.Hostname() == host {
					t.Fatalf("%s kept an ambiguous host", key)
				}
				if strings.Contains(original[key], "@") {
					password, _ := u.User.Password()
					if password != host {
						t.Fatalf("%s URL password changed", key)
					}
				}
			}
		})
	}
	unchanged := "postgres://postgres:postgres@remote.example:5432/postgres?name=postgres"
	got := rewriteBindingHost(map[string]string{"DATABASE_URL": unchanged}, "postgres", "donkey.one.postgres")
	if got["DATABASE_URL"] != unchanged {
		t.Fatal("external URL was rewritten")
	}
}

func TestServiceProxyUsesSpaceNetworkIdentity(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}}
	for space, subnets := range map[string][]network.IPAMConfig{
		"team":  {{Subnet: "10.20.0.0/24"}, {Subnet: "fd00:1::/64"}},
		"other": {{Subnet: "10.30.0.0/24"}},
	} {
		if err := r.recordProxyNetwork(space, network.IPAM{Config: subnets}); err != nil {
			t.Fatal(err)
		}
	}
	binding := map[string]string{addons.ProxyEnvKey("erp"): addons.ProxyBindingURL(r.serviceProxyURL(), "erp")}
	if err := r.state.setBinding("team", sharedOwner, "allowed", binding); err != nil {
		t.Fatal(err)
	}
	if err := r.state.setBinding("other", "app", "copy", binding); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ip, id  string
		allowed bool
	}{
		{"10.20.0.2", "erp", true}, {"fd00:1::2", "erp", true},
		{"10.30.0.2", "erp", false}, {"10.20.0.2", "other", false}, {"10.40.0.2", "erp", false},
	} {
		got, err := r.ServiceProxyAccess(context.Background(), tc.ip, tc.id)
		if err != nil || got != tc.allowed {
			t.Errorf("%s/%s access = %v, %v", tc.ip, tc.id, got, err)
		}
	}
	if err := r.state.writeJSON(addons.Metadata{Type: "erp", Protocol: "http"}, "service-metadata", "team", "allowed.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveService(context.Background(), "team", "allowed", true, false); err == nil {
		t.Fatal("proxy access cannot be retained as data")
	}
	if err := r.state.removeBinding("team", sharedOwner, "allowed"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ServiceProxyAccess(context.Background(), "10.20.0.2", "erp"); err != nil || got {
		t.Fatalf("removed grant remained authorized: %v %v", got, err)
	}
	if err := r.state.writeJSON(proxyNetwork{Updated: time.Now().Add(-time.Minute), CIDRs: []string{"10.20.0.0/24"}}, "proxy", "networks", "team.json"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ServiceProxyAccess(context.Background(), "10.20.0.2", "erp"); err == nil || got {
		t.Fatalf("stale network accepted: %v %v", got, err)
	}
}

func TestExaProxyCredentials(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{AIContainer: "donkey-ai"}}
	ctx := context.Background()
	for _, fixture := range []struct{ name, endpoint, key, owner string }{
		{"platform", r.exaProxyURL(), "platform-token", sharedOwner},
		{"vendor", "https://vendor.example/v1", "vendor-secret", sharedOwner},
		{"copy", r.exaProxyURL(), "app-token", "api"},
	} {
		if err := r.state.setBinding("team", fixture.owner, fixture.name, map[string]string{"EXA_BASE_URL": fixture.endpoint, "EXA_API_KEY": fixture.key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.state.writeJSON(addons.Metadata{Type: "external-model", Protocol: "custom"}, "service-metadata", "team", "vendor.json"); err != nil {
		t.Fatal(err)
	}
	connection, err := r.ServiceConnection(ctx, "team", "vendor")
	if err != nil || connection.Type != "external-model" || connection.Protocol != "custom" {
		t.Fatalf("provider metadata: %+v %v", connection, err)
	}
	tokens, err := r.ExaProxyTokens(ctx)
	if err != nil || !reflect.DeepEqual(tokens, []string{"platform-token"}) {
		t.Fatalf("wrong proxy tokens: %v", err)
	}
	for _, folder := range []string{"env", "bindings", "provisioning"} {
		if err := r.state.writeJSON(map[string]string{"secret": "private"}, folder, "team", "private.json"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(r.state.path(folder, "team", "private.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s credentials are not private: %v", folder, err)
		}
	}
}
