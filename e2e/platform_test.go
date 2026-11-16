//go:build e2e

package e2e

import (
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/server"
)

// The platform page's endpoints against a running installation — on
// whichever runtime it is: the page is the same on a cluster and on a
// Docker engine, so the same assertions hold for both. It only reads; what
// is deployed is whatever the installation has.
//
//	hack/verify.sh kind -run '^TestPlatformPage$' -v
func TestPlatformPage(t *testing.T) {
	s := newSuite(t)

	// fetch reads a whole answer: a report of a busy installation is larger
	// than what the suite's other calls expect.
	fetch := func(t *testing.T, path string, out any) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, s.server+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d — %s", path, resp.StatusCode, body)
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
		}
		return resp, body
	}

	t.Run("the control plane describes itself", func(t *testing.T) {
		var system server.SystemResponse
		fetch(t, "/v1/system", &system)
		if system.Runtime.Name != "Kubernetes" && system.Runtime.Name != "Docker" {
			t.Fatalf("runtime: %+v", system.Runtime)
		}
		if system.Runtime.Version == "" || system.Domain != s.domain || system.ComponentsError != "" {
			t.Fatalf("system: %+v", system)
		}
		// The parts every installation has, in the one order both runtimes
		// list them, each of them running — this test is talking to them.
		var roles []string
		for _, component := range system.Components {
			roles = append(roles, component.Role)
			if component.Name == "" || component.Want < 1 {
				t.Errorf("component: %+v", component)
			}
		}
		at := func(role string) int { return slices.Index(roles, role) }
		if at("server") != 0 || at("orchestrator") != 1 || at("gateway") != 2 {
			t.Fatalf("the control plane's parts: %v", roles)
		}
		for _, component := range system.Components[:3] {
			if !component.Healthy || component.Image == "" || component.StartedAt == "" || component.Message != "" {
				t.Errorf("%s is listed as not running: %+v", component.Name, component)
			}
		}
	})

	t.Run("the nodes say what they have and what is in use", func(t *testing.T) {
		var list server.NodesResponse
		fetch(t, "/v1/nodes", &list)
		if list.Note != "" {
			// An installation confined to namespaces it was given: saying so
			// is the answer.
			if len(list.Nodes) != 0 {
				t.Fatalf("a note and nodes: %+v", list)
			}
			t.Skipf("the nodes are not this installation's to see: %s", list.Note)
		}
		if len(list.Nodes) == 0 {
			t.Fatal("no node, and no note saying why")
		}
		ready := 0
		for _, node := range list.Nodes {
			if node.Name == "" || node.CPU.Capacity <= 0 || node.Memory.Capacity <= 0 {
				t.Errorf("node: %+v", node)
			}
			if node.Ready {
				ready++
			}
			// A reading, where there is one, is a share of what the node
			// has. Usage can sit a little over what workloads may have (the
			// system's own share); never over by a multiple.
			for name, resource := range map[string]server.SystemNodeResource{"cpu": node.CPU, "memory": node.Memory} {
				if resource.Used != nil && (*resource.Used < 0 || *resource.Used > 2*resource.Capacity) {
					t.Errorf("%s %s in use: %v of %v", node.Name, name, *resource.Used, resource.Capacity)
				}
				if resource.Reserved != nil && *resource.Reserved < 0 {
					t.Errorf("%s %s reserved: %v", node.Name, name, *resource.Reserved)
				}
			}
		}
		// Donkey runs on one of them.
		if ready == 0 {
			t.Fatalf("no node is ready: %+v", list.Nodes)
		}
	})

	t.Run("findings are grouped by the finding, with the apps they are in", func(t *testing.T) {
		var report server.VulnerabilityReport
		fetch(t, "/v1/vulnerabilities", &report)
		var fleet struct {
			Apps []struct{ Space, Name string } `json:"apps"`
		}
		fetch(t, "/v1/apps", &fleet)
		running := map[string]bool{}
		for _, app := range report.Apps {
			running[app.Space+"/"+app.App] = true
		}
		// Every app the fleet lists is one the report covers, scanned or not.
		for _, app := range fleet.Apps {
			if !running[app.Space+"/"+app.Name] {
				t.Errorf("%s/%s runs and is missing from the report", app.Space, app.Name)
			}
		}
		rank := map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}
		rows := 0
		for i, finding := range report.Findings {
			if finding.ID == "" || len(finding.Apps) == 0 {
				t.Fatalf("finding: %+v", finding)
			}
			if i > 0 && rank[finding.Severity] > rank[report.Findings[i-1].Severity] {
				t.Fatalf("%s (%s) is listed after %s (%s)", finding.ID, finding.Severity,
					report.Findings[i-1].ID, report.Findings[i-1].Severity)
			}
			for _, affected := range finding.Apps {
				if !running[affected.Space+"/"+affected.App] || affected.State == "" {
					t.Errorf("%s is in %s/%s, which the report does not know: %+v", finding.ID, affected.Space, affected.App, affected)
				}
				if _, ok := report.Spaces[affected.Space]; !ok {
					t.Errorf("no contacts for the space %s", affected.Space)
				}
				rows += max(1, len(affected.Packages)+len(affected.Locations))
			}
		}

		// The export is the same report, a row per finding, app and package.
		resp, body := fetch(t, "/v1/vulnerabilities?format=csv", nil)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(resp.Header.Get("Content-Disposition"), "donkey-vulnerabilities-") {
			t.Fatalf("export headers: %+v", resp.Header)
		}
		records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xEF\xBB\xBF"))).ReadAll()
		if err != nil || len(records) == 0 || records[0][0] != "kind" || records[0][1] != "id" {
			t.Fatalf("export: %v %v", err, records[:min(len(records), 1)])
		}
		// Another deploy or scan can land between the two reads, so the
		// count is held to the report only when nothing changed.
		var again server.VulnerabilityReport
		fetch(t, "/v1/vulnerabilities", &again)
		if again.GeneratedAt == report.GeneratedAt && len(records)-1 != rows {
			t.Fatalf("the export has %d rows, the report %d", len(records)-1, rows)
		}
	})

	t.Run("technologies are listed by category, most used first", func(t *testing.T) {
		var report server.TechnologyReport
		fetch(t, "/v1/technologies", &report)
		var ids []string
		for _, category := range report.Categories {
			ids = append(ids, category.ID)
			for i, technology := range category.Technologies {
				if technology.Name == "" || len(technology.Apps) == 0 {
					t.Errorf("%s: %+v", category.ID, technology)
				}
				if i > 0 && len(technology.Apps) > len(category.Technologies[i-1].Apps) {
					t.Errorf("%s: %s is used more than %s listed before it", category.ID, technology.Name, category.Technologies[i-1].Name)
				}
			}
		}
		if strings.Join(ids, ",") != "language,framework,base-image,os,package-manager" {
			t.Fatalf("categories: %v", ids)
		}
		resp, body := fetch(t, "/v1/technologies?format=csv", nil)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
			!strings.HasPrefix(strings.TrimPrefix(string(body), "\xEF\xBB\xBF"), "category,technology,") {
			t.Fatalf("export: %+v %q", resp.Header, body[:min(len(body), 80)])
		}
	})

	// Someone who is signed in and is not a platform admin sees none of it.
	t.Run("the page is for platform admins", func(t *testing.T) {
		// An installation's first sign-in becomes its admin. The suite's own
		// user has to be the one who was first, never the stranger.
		fetch(t, "/v1/ping", nil)
		key, err := hex.DecodeString(os.Getenv("DONKEY_E2E_SESSION_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		stranger, err := identity.SealToken(key, identity.Identity{
			Subject: "e2e-stranger",
			Email:   "stranger@e2e.invalid",
			Expires: time.Now().Add(10 * time.Minute).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/v1/system", "/v1/nodes", "/v1/vulnerabilities", "/v1/technologies"} {
			req, err := http.NewRequest(http.MethodGet, s.server+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+stranger)
			resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("GET %s as somebody who is no platform admin: %d", path, resp.StatusCode)
			}
		}
	})
}
