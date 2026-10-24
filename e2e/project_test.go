//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/server"
)

// Upload upstream source without Dockerfiles and let the live assistant
// discover, build and connect every app.
func TestProjectGoogle(t *testing.T) {
	s := newSuite(t)
	dir := os.Getenv("DONKEY_E2E_GOOGLE")
	if dir == "" {
		t.Skip("DONKEY_E2E_GOOGLE not set — run task verify:monorepos")
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.Contains(name, "dockerfile") || strings.Contains(name, "containerfile") || strings.HasSuffix(name, "dockerignore") {
			t.Errorf("upstream build file remains: %s; run hack/prepare-monorepos.sh", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	apps := []string{"src/adservice", "src/cartservice/src", "src/checkoutservice", "src/currencyservice", "src/emailservice", "src/frontend", "src/paymentservice", "src/productcatalogservice", "src/recommendationservice", "src/shippingservice"}
	plan, result := s.deployProject(t, dir, "e2e-boutique", apps, "src/frontend")
	redis := false
	for _, app := range plan.Apps {
		for _, service := range app.Services {
			redis = redis || service.Type == "redis" && service.Selected
		}
	}
	if !redis {
		t.Fatal("cartservice's Redis dependency was not discovered")
	}
	s.assertProjectHealthy(t, result)
	if s.edge == nil {
		return
	}
	base := ""
	for _, app := range plan.Apps {
		if app.Root == "src/frontend" {
			base = "https://" + app.App.Name + ".e2e-boutique." + s.domain
		}
	}
	s.waitBody(t, base+"/", "Online Boutique", 2*time.Minute)
	client := *s.edge
	client.Jar, _ = cookiejar.New(nil)
	request := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return res.StatusCode, string(body)
	}
	request(http.MethodGet, "/", nil)
	status, body := request(http.MethodPost, "/cart", url.Values{"product_id": {"OLJCESPC7Z"}, "quantity": {"2"}})
	if status < 200 || status >= 400 {
		t.Fatalf("add to cart: %d %s", status, body)
	}
	status, body = request(http.MethodGet, "/cart", nil)
	if status != http.StatusOK || !strings.Contains(body, "Sunglasses") {
		t.Fatalf("Redis-backed cart did not retain the product: %d %s", status, body)
	}
	status, body = request(http.MethodPost, "/cart/checkout", url.Values{
		"email": {"e2e@example.invalid"}, "street_address": {"123 Test Street"}, "zip_code": {"8000"},
		"city": {"Zurich"}, "state": {"ZH"}, "country": {"Switzerland"},
		"credit_card_number": {"4111111111111111"}, "credit_card_expiration_month": {"12"},
		"credit_card_expiration_year": {"2030"}, "credit_card_cvv": {"123"},
	})
	if status != http.StatusOK || !strings.Contains(body, "Your order is complete") {
		t.Fatalf("checkout across gRPC services failed: %d %s", status, body)
	}
	t.Log("Google demo: browsing, Redis-backed cart, payment, shipping and checkout passed")
}

func (s *suite) deployProject(t *testing.T, dir, space string, expected []string, exposed string) (*server.ProjectPlan, *server.DeployResult) {
	t.Helper()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("missing monorepo %s; run hack/prepare-monorepos.sh", dir)
	}
	var archive bytes.Buffer
	if _, err := apiclient.TarContext(dir, &archive); err != nil {
		t.Fatal(err)
	}
	// These dedicated test spaces may be replaced on reruns. Keep them live
	// afterwards when asked, so the console and apps remain inspectable.
	s.rawTry(http.MethodDelete, "/v1/spaces/"+space)
	s.raw(t, http.MethodPost, "/v1/spaces", map[string]any{"name": space, "allowPublic": true, "viewers": map[string]any{"users": []string{"demo@example.com"}}}, nil)
	if os.Getenv("DONKEY_E2E_KEEP") != "true" {
		t.Cleanup(func() { s.rawTry(http.MethodDelete, "/v1/spaces/"+space) })
	}
	upload := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, s.server+path, bytes.NewReader(archive.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Content-Type", "application/gzip")
		res, err := (&http.Client{Timeout: 50 * time.Minute}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			defer res.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			t.Fatalf("%s: %d %s", path, res.StatusCode, data)
		}
		return res
	}
	t.Logf("Reading %s (%d bytes)…", filepath.Base(dir), archive.Len())
	response := upload("/v1/projects/plans?space=" + space)
	var plan server.ProjectPlan
	if err := json.NewDecoder(response.Body).Decode(&plan); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	saveProjectPlan(t, space, plan)
	var names []string
	var edits []server.PlanEdit
	for _, app := range plan.Apps {
		if app.Build.Strategy != string(config.BuildAI) {
			t.Fatalf("%s uses %q; every app must be built by the AI", app.Root, app.Build.Strategy)
		}
		names = append(names, app.Root)
		edit := server.PlanEdit{Name: app.App.Name, Space: space, Port: app.App.Port, Healthcheck: app.App.Healthcheck, Storage: app.App.Storage, Exposure: "internal"}
		if app.Root == exposed {
			edit.Exposure = "public"
		}
		for _, service := range app.Services {
			if service.Selected {
				edit.Services = append(edit.Services, service.Name)
			}
		}
		edits = append(edits, edit)
	}
	slices.Sort(names)
	expected = slices.Clone(expected)
	slices.Sort(expected)
	if !slices.Equal(names, expected) {
		t.Fatalf("discovered %v, want %v", names, expected)
	}
	s.raw(t, http.MethodPut, "/v1/projects/plans/"+plan.ID, edits, &plan)
	saveProjectPlan(t, space, plan)
	if !plan.Ready {
		data, _ := json.MarshalIndent(plan, "", "  ")
		t.Fatalf("project has blocking findings: %s", data)
	}
	t.Logf("Reviewed %d apps, %d connections, %d source fixes", len(plan.Apps), len(plan.Connections), len(plan.Changes))
	response = upload("/v1/projects/deploys?plan=" + plan.ID)
	defer response.Body.Close()
	var stream io.Reader = response.Body
	if reports := os.Getenv("DONKEY_E2E_REPORTS"); reports != "" {
		f, err := os.Create(filepath.Join(reports, space+"-deploy.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		stream = io.TeeReader(stream, f)
	}
	decoder := json.NewDecoder(stream)
	dockerfiles := map[string]string{}
	currentApp := ""
	for {
		var message struct {
			Line   string               `json:"line"`
			Update *server.DeployUpdate `json:"update"`
			Result *server.DeployResult `json:"result"`
		}
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("deployment stream ended without a result: %v", err)
		}
		if update := message.Update; update != nil {
			if update.App != "" {
				currentApp = update.App
			}
			if update.App != "" && update.Stage != "" {
				t.Logf("%s: %s", update.App, update.Stage)
			}
			if update.Dockerfile != "" {
				dockerfiles[currentApp] = update.Dockerfile
			}
			for _, base := range update.BaseImages {
				if strings.Contains(base, "/microservices-demo/") || strings.Contains(base, "online-boutique") || strings.Contains(base, "hipster-shop") {
					t.Errorf("%s reused an upstream app image: %s", currentApp, base)
				}
			}
		}
		if strings.HasPrefix(message.Line, "Deploying ") || strings.HasPrefix(message.Line, "✓ ") {
			t.Log(message.Line)
		}
		if result := message.Result; result != nil {
			for _, app := range result.Apps {
				if !app.OK {
					t.Errorf("%s failed: %s\n%s", app.App, app.Error, app.Diagnosis)
				}
				if dockerfiles[app.App] == "" {
					t.Errorf("%s did not report its generated Dockerfile", app.App)
				}
			}
			if !result.OK || len(result.Apps) != len(expected) {
				t.Fatalf("not all apps deployed: %s (%d/%d)", result.Error, len(result.Apps), len(expected))
			}
			return &plan, result
		}
	}
}

func (s *suite) assertProjectHealthy(t *testing.T, result *server.DeployResult) {
	t.Helper()
	for _, app := range result.Apps {
		status, err := s.api.Status(context.Background(), app.Space, app.App)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Found || !status.Healthy {
			t.Fatalf("%s is not healthy after deployment: %+v", app.App, status)
		}
		t.Logf("%s is deployed in %s", app.App, app.Space)
	}
}

func saveProjectPlan(t *testing.T, space string, plan server.ProjectPlan) {
	t.Helper()
	if reports := os.Getenv("DONKEY_E2E_REPORTS"); reports != "" {
		if err := os.MkdirAll(reports, 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.MarshalIndent(plan, "", "  ")
		if err := os.WriteFile(filepath.Join(reports, space+"-plan.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
