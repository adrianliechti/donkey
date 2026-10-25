//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/server"
)

// The vulnerability gate against a running installation: an app that uses
// a package with a known critical is refused before anything is pushed, a
// credential baked into an image is refused, a space admin's waiver lets
// the image through and is recorded with the release, a clean app deploys
// with its scan on record, and doctor reports the scanner and its database.
//
//	hack/verify.sh docker -run '^TestScanGate$' -v
func TestScanGate(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	// DONKEY_E2E_SCAN_SPACE names an existing space to deploy into, for a
	// Docker host that has no address pool left for one more network; the
	// suite then neither creates nor deletes the space.
	scanSpace, ownSpace := os.Getenv("DONKEY_E2E_SCAN_SPACE"), false
	if scanSpace == "" {
		scanSpace, ownSpace = "e2e-scan", true
	}
	const (
		vulnApp  = "vuln-web"
		leakyApp = "leaky-web"
		cleanApp = "clean-web"
	)

	deploy := func(t *testing.T, name, dockerfile string, files map[string]string, opts apiclient.DeployOpts) (*server.DeployResult, string) {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
		for path, content := range files {
			writeFile(t, filepath.Join(dir, path), content)
		}
		writeFile(t, filepath.Join(dir, "donkey.yaml"), fmt.Sprintf("name: %s\nspace: %s\nport: 8080\n", name, scanSpace))
		var tarBuf bytes.Buffer
		if _, err := apiclient.TarContext(dir, &tarBuf); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		var mu sync.Mutex
		var lines strings.Builder
		opts.Space = scanSpace
		res, err := s.api.Deploy(deadline, &tarBuf, opts, func(line string) {
			t.Log(line)
			mu.Lock()
			lines.WriteString(line + "\n")
			mu.Unlock()
		}, func(string) {})
		if err != nil {
			t.Fatalf("deploy did not run: %v", err)
		}
		return res, lines.String()
	}

	latestScan := func(t *testing.T, name string) *server.AppScan {
		t.Helper()
		var history struct {
			Builds []struct {
				Sequence int64 `json:"sequence"`
			} `json:"builds"`
		}
		s.raw(t, http.MethodGet, fmt.Sprintf("/v1/spaces/%s/apps/%s/builds", scanSpace, name), nil, &history)
		if len(history.Builds) == 0 {
			t.Fatal("no release on record")
		}
		var release server.AppRelease
		s.raw(t, http.MethodGet, fmt.Sprintf("/v1/spaces/%s/apps/%s/builds/%d", scanSpace, name, history.Builds[0].Sequence), nil, &release)
		if release.Scan == nil {
			t.Fatal("the release record carries no scan")
		}
		return release.Scan
	}

	// forgetWaivers drops the test apps' waivers from the space settings: an
	// interrupted earlier run leaves its waiver behind, and a waived key is
	// let through — correctly, which is not what the refusal test is for.
	forgetWaivers := func(t *testing.T) {
		t.Helper()
		var settings config.SpaceConfig
		resp := s.raw(t, http.MethodGet, "/v1/spaces/"+scanSpace+"/settings", nil, &settings)
		if resp == nil {
			return
		}
		kept := []config.SecurityWaiver{}
		for _, w := range settings.SecurityWaivers {
			if w.App != vulnApp && w.App != leakyApp && w.App != cleanApp {
				kept = append(kept, w)
			}
		}
		if len(kept) != len(settings.SecurityWaivers) {
			settings.SecurityWaivers = kept
			s.raw(t, http.MethodPut, "/v1/spaces/"+scanSpace+"/settings", settings, nil)
		}
	}

	t.Run("clean-slate", func(t *testing.T) {
		for _, name := range []string{vulnApp, leakyApp, cleanApp} {
			_ = s.api.Destroy(ctx, scanSpace, name, false)
		}
		if ownSpace {
			s.rawTry(http.MethodDelete, "/v1/spaces/"+scanSpace)
			s.raw(t, http.MethodPost, "/v1/spaces", map[string]any{"name": scanSpace, "viewers": map[string]any{"users": []string{"demo@example.com"}}}, nil)
		} else {
			forgetWaivers(t)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		checks, _, err := s.api.Doctor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range checks {
			for _, want := range []string{"Vulnerability scanner", "Vulnerability database"} {
				if strings.HasPrefix(c.Name, want) {
					seen[want] = true
					if !c.OK {
						t.Errorf("%s: %s", c.Name, c.Detail)
					}
					t.Logf("%s — %s", c.Name, c.Detail)
				}
			}
		}
		if len(seen) != 2 {
			t.Errorf("doctor should report the scanner and its database, saw %v", seen)
		}
	})

	// lodash 4.17.11 carries CVE-2019-10744 (critical, fixed in 4.17.12): a
	// prototype pollution in defaultsDeep. This app installs the real
	// package and calls exactly that function on request bodies — affected
	// by any honest reading, which is what the assistant is asked for.
	vulnRecipe := `FROM node:22-alpine
WORKDIR /app
RUN npm install --no-audit --no-fund --ignore-scripts lodash@4.17.11
COPY server.js .
CMD ["node", "server.js"]
`
	vulnServer := `const http = require('http');
const _ = require('lodash');
const defaults = { theme: 'light', pageSize: 20 };
// Settings arrive as JSON and are deep-merged over the defaults with
// lodash's defaultsDeep — the function CVE-2019-10744 is about.
http.createServer((req, res) => {
  let body = '';
  req.on('data', (chunk) => (body += chunk));
  req.on('end', () => {
    const settings = _.defaultsDeep(body ? JSON.parse(body) : {}, defaults);
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify(settings));
  });
}).listen(Number(process.env.PORT) || 8080, '0.0.0.0');
`
	t.Run("a critical with a fix is refused", func(t *testing.T) {
		res, lines := deploy(t, vulnApp, vulnRecipe, map[string]string{"server.js": vulnServer}, apiclient.DeployOpts{})
		if res.OK {
			t.Fatalf("the deploy should have been refused: %+v", res)
		}
		if !strings.Contains(res.Error, "refused by the vulnerability policy") {
			t.Errorf("error = %q", res.Error)
		}
		if !strings.Contains(lines, "CVE-2019-10744") || !strings.Contains(lines, "lodash 4.17.11") {
			t.Errorf("the deploy should name the finding:\n%s", lines)
		}
		if !strings.Contains(lines, "asking the assistant") {
			t.Errorf("the assistant should have been asked:\n%s", lines)
		}
		if st, err := s.api.Status(ctx, scanSpace, vulnApp); err == nil && st.Found {
			t.Errorf("nothing should be running: %+v", st)
		}
	})

	leakyRecipe := func(secret bool) string {
		key := ""
		if secret {
			// Credential-shaped, not a credential.
			key = ` && printf 'AWS_ACCESS_KEY_ID=AKIAQWERTYUIOPASDFGH\n' > /site/settings.env`
		}
		return "FROM busybox:1.37\nRUN mkdir -p /site && echo leaky > /site/index.html" + key + "\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/site\"]\n"
	}
	t.Run("a credential in the image is refused", func(t *testing.T) {
		res, lines := deploy(t, leakyApp, leakyRecipe(true), nil, apiclient.DeployOpts{})
		if res.OK {
			t.Fatalf("the deploy should have been refused: %+v", res)
		}
		if !strings.Contains(res.Error, "refused by the vulnerability policy") || !strings.Contains(lines, "credential is baked into the image") {
			t.Errorf("error = %q\n%s", res.Error, lines)
		}
	})

	t.Run("a space admin's waiver lets it through, on the record", func(t *testing.T) {
		var settings config.SpaceConfig
		s.raw(t, http.MethodGet, "/v1/spaces/"+scanSpace+"/settings", nil, &settings)
		others := []config.SecurityWaiver{}
		for _, w := range settings.SecurityWaivers {
			if w.App != leakyApp {
				others = append(others, w)
			}
		}
		settings.SecurityWaivers = append(append([]config.SecurityWaiver{}, others...), config.SecurityWaiver{
			App: leakyApp, ID: "aws-access-key-id", Reason: "a fixture key for the e2e suite",
			Expires: time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02"),
		})
		s.raw(t, http.MethodPut, "/v1/spaces/"+scanSpace+"/settings", settings, nil)

		res, lines := deploy(t, leakyApp, leakyRecipe(true), nil, apiclient.DeployOpts{})
		if !res.OK {
			t.Fatalf("the waived deploy should go through: %s\n%s", res.Error, lines)
		}
		if !strings.Contains(lines, "waived by "+s.user) {
			t.Errorf("the deploy should say who waived it:\n%s", lines)
		}
		scan := latestScan(t, leakyApp)
		if scan.Blocked || scan.Summary.Waived != 1 || scan.Summary.Secrets != 1 {
			t.Errorf("recorded scan = blocked %v, summary %+v", scan.Blocked, scan.Summary)
		}
		// The running app carries the outcome as its badge.
		if st, err := s.api.Status(ctx, scanSpace, leakyApp); err != nil || st.Security == nil || st.Security.State != server.SecurityWaived {
			t.Errorf("status security = %+v, %v", st.Security, err)
		}
		if len(scan.Policy.Waivers) != 1 || scan.Policy.Waivers[0].By != s.user {
			t.Errorf("the record should carry the waiver with its author: %+v", scan.Policy.Waivers)
		}

		// Reusing the image re-reads the record against the policy of the
		// day: without the waiver, the same image is refused again.
		settings.SecurityWaivers = others
		s.raw(t, http.MethodPut, "/v1/spaces/"+scanSpace+"/settings", settings, nil)
		st, err := s.api.Status(ctx, scanSpace, leakyApp)
		if err != nil || st.Image == "" {
			t.Fatalf("status = %+v, %v", st, err)
		}
		res, lines = deploy(t, leakyApp, leakyRecipe(true), nil, apiclient.DeployOpts{NoBuild: true, Tag: st.Image})
		if res.OK || !strings.Contains(res.Error, "refused by the vulnerability policy") {
			t.Errorf("a rollback to a refused image should be refused: ok=%v %q\n%s", res.OK, res.Error, lines)
		}
	})

	t.Run("a clean app deploys with its scan on record", func(t *testing.T) {
		res, lines := deploy(t, cleanApp, leakyRecipe(false), nil, apiclient.DeployOpts{})
		if !res.OK {
			t.Fatalf("deploy failed: %s\n%s", res.Error, lines)
		}
		scan := latestScan(t, cleanApp)
		if scan.Blocked || scan.Summary.Blocking != 0 || scan.Scanner == "" {
			t.Errorf("recorded scan = %+v", scan.Summary)
		}
		if scan.DBUpdatedAt == "" || scan.ScannedAt == "" {
			t.Error("the record should say how fresh the database was and when the image was read")
		}
		st, err := s.api.Status(ctx, scanSpace, cleanApp)
		if err != nil || st.Security == nil || st.Security.State != server.SecurityClean || st.Security.ScannedAt == "" {
			t.Errorf("status security = %+v, %v", st.Security, err)
		}
		apps, err := s.api.Apps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := false
		for _, a := range apps {
			if a.Space == scanSpace && a.Name == cleanApp {
				listed = a.Security != nil && a.Security.State == server.SecurityClean
			}
		}
		if !listed {
			t.Error("the overview should carry the badge too")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		for _, name := range []string{vulnApp, leakyApp, cleanApp} {
			if err := s.api.Destroy(ctx, scanSpace, name, false); err != nil {
				t.Fatal(err)
			}
		}
		var board struct {
			Deploys []server.ActiveDeploy `json:"deploys"`
		}
		s.raw(t, http.MethodGet, "/v1/deploys", nil, &board)
		for _, deploy := range board.Deploys {
			if deploy.Space == scanSpace && (deploy.App == vulnApp || deploy.App == leakyApp || deploy.App == cleanApp) {
				t.Errorf("removed test app still appears on the deployment board: %+v", deploy)
			}
		}
		if ownSpace {
			s.rawTry(http.MethodDelete, "/v1/spaces/"+scanSpace)
		} else {
			forgetWaivers(t)
		}
	})
}
