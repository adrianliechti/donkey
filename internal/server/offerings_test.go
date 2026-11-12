package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
)

type configuredCatalog struct {
	addonv1.UnimplementedServiceProviderServer
}

func (configuredCatalog) Offerings(context.Context, *addonv1.OfferingsRequest) (*addonv1.OfferingsResponse, error) {
	schema := addons.ObjectSchema(map[string]any{"tenant": map[string]any{"type": "string", "pattern": "^[a-z]+$"}})
	schema["required"] = []any{"tenant"}
	return &addonv1.OfferingsResponse{Offerings: []*addonv1.Offering{{Id: "private-api", Title: "Private API", Protocol: "custom", SpaceConfiguration: addons.MustStruct(schema), Env: []*addonv1.EnvVar{{Name: "API_URL"}}}}}, nil
}

func TestSpaceCatalogShowsConfigurationReadinessWithoutAdminValues(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remote := grpc.NewServer()
	addonv1.RegisterServiceProviderServer(remote, configuredCatalog{})
	go remote.Serve(listener)
	t.Cleanup(remote.Stop)
	closePlugins, err := addons.ConnectPlugins(config.AddonPlugins{{Name: "private", Address: listener.Addr().String(), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlugins)
	s, r := spaceTestServer()
	s.cfg.CommonOfferings = config.CommonOfferings{"postgres"}
	settings := r.settings["team"]
	settings.Offerings = map[string]config.SpaceOffering{"private-api": {}}
	r.settings["team"] = settings
	response := callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/catalog", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Needs configuration") {
		t.Fatalf("incomplete config not reported: %d %s", response.Code, response.Body)
	}
	settings.Offerings["private-api"] = config.SpaceOffering{Config: map[string]any{"tenant": "finance"}}
	body, _ := json.Marshal(settings)
	response = callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body)
	if response.Code != http.StatusOK {
		t.Fatalf("valid admin configuration rejected: %d %s", response.Code, response.Body)
	}
	response = callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/catalog", nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "finance") || strings.Contains(response.Body.String(), "Needs configuration") {
		t.Fatalf("config leaked or readiness stale: %d %s", response.Code, response.Body)
	}
}

func TestSpaceCatalogFiltersOfferingsAndAppliesDefaults(t *testing.T) {
	s, r := spaceTestServer()
	s.cfg.CommonOfferings = config.CommonOfferings{"postgres"}
	settings := r.settings["team"]
	settings.Offerings = map[string]config.SpaceOffering{"redis": {Defaults: map[string]any{"storage": "10Gi"}}}
	r.settings["team"] = settings
	response := callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/catalog", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", response.Code, response.Body)
	}
	var catalog struct {
		Types []struct {
			Type       string `json:"type"`
			Parameters struct {
				Properties map[string]struct {
					Default any `json:"default"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"types"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Types) != 2 {
		t.Fatalf("unavailable offerings leaked into catalog: %s", response.Body)
	}
	for _, entry := range catalog.Types {
		if entry.Type != "postgres" && entry.Type != "redis" {
			t.Fatal("wrong offering:", entry.Type)
		}
		if entry.Type == "redis" && entry.Parameters.Properties["storage"].Default != "10Gi" {
			t.Fatal("space defaults missing from form")
		}
	}
	s.cfg.CommonOfferings = nil
	response = callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/catalog", nil)
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Types) != 5 {
		t.Fatal("empty commonOfferings did not allow all builtins")
	}
	response = callAs(t, s, "dev@example.com", "GET", "/v1/spaces/other/catalog", nil)
	if response.Code != http.StatusForbidden {
		t.Fatal("space catalog bypassed membership")
	}
}

func TestOnlyAdminsCanConfigureSpaceOfferings(t *testing.T) {
	s, r := spaceTestServer()
	settings := r.settings["team"]
	settings.Offerings = map[string]config.SpaceOffering{"postgres": {Defaults: map[string]any{"storage": "10Gi"}}}
	body, _ := json.Marshal(settings)
	response := callAs(t, s, "dev@example.com", "PUT", "/v1/spaces/team/settings", body)
	if response.Code != http.StatusForbidden || r.writes != 0 {
		t.Fatal("developer changed offerings")
	}
	response = callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body)
	if response.Code != http.StatusOK || r.settings["team"].Offerings["postgres"].Defaults["storage"] != "10Gi" {
		t.Fatalf("admin edit: %d %s", response.Code, response.Body)
	}
	for _, entry := range []config.SpaceOffering{
		{Defaults: map[string]any{"unknown": "value"}},
		{Defaults: map[string]any{"storage": "bad"}},
		{Config: map[string]any{"tenant": "finance"}},
	} {
		settings.Offerings["postgres"] = entry
		body, _ = json.Marshal(settings)
		response = callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid setting accepted: %s", response.Body)
		}
	}
	// A provider removed from the installation must not block membership edits.
	settings.Offerings = map[string]config.SpaceOffering{"retired-provider": {Config: map[string]any{"tenant": "finance"}}}
	r.settings["team"] = settings
	settings.Developers.Users = []string{"new@example.com"}
	body, _ = json.Marshal(settings)
	response = callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "new@example.com") {
		t.Fatal("unrelated edit blocked by retired provider:", response.Body)
	}
}

func TestLegacyS3SettingsAndCatalogRemainEditable(t *testing.T) {
	s, r := spaceTestServer()
	s.cfg.CommonOfferings = config.CommonOfferings{"postgres"}
	settings := r.settings["team"]
	settings.Offerings = map[string]config.SpaceOffering{"rustfs": {Defaults: map[string]any{"storage": "5Gi"}}}
	r.settings["team"] = settings
	response := callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/catalog", nil)
	var catalog struct {
		Types []struct {
			Type       string `json:"type"`
			Parameters struct {
				Properties map[string]struct {
					Default any `json:"default"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"types"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range catalog.Types {
		if entry.Type == "rustfs" {
			t.Fatal("legacy catalog entry duplicated S3")
		}
		if entry.Type == "s3" {
			found = true
			if entry.Parameters.Properties["storage"].Default != "5Gi" {
				t.Fatal("catalog lost legacy storage default")
			}
		}
	}
	if !found {
		t.Fatalf("legacy space grant lost S3: %s", response.Body)
	}
	response = callAs(t, s, "owner@example.com", "GET", "/v1/spaces/team/settings", nil)
	var exposed config.SpaceConfig
	if response.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", response.Code, response.Body)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &exposed); err != nil {
		t.Fatal(err)
	}
	if _, old := exposed.Offerings["rustfs"]; old || exposed.Offerings["s3"].Defaults["storage"] != "5Gi" {
		t.Fatal("UI cannot edit S3 settings through the canonical id")
	}
	body, _ := json.Marshal(exposed)
	response = callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body)
	if response.Code != http.StatusOK || r.settings["team"].Offerings["s3"].Defaults["storage"] != "5Gi" {
		t.Fatalf("saving canonical settings lost defaults: %d %s", response.Code, response.Body)
	}
}
