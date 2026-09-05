package docker

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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

func TestBindingHostRewritePreservesCredentialsAndPaths(t *testing.T) {
	for _, kind := range []string{"postgres", "redis", "rustfs"} {
		t.Run(kind, func(t *testing.T) {
			addon, _ := addons.Get(kind)
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
