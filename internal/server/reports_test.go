package server

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// reportRuntime is a fleet: apps with a status each, spaces with settings.
type reportRuntime struct {
	runtime.Provider
	apps   map[runtime.AppRef]runtime.AppStatus
	spaces map[string]config.SpaceConfig
	broken map[runtime.AppRef]bool
	// statusReads counts Status calls, to see what a second report costs.
	// The inventory reads several apps at once.
	statusReads atomic.Int64
}

func (r *reportRuntime) PlatformConfig(context.Context) (config.PlatformConfig, error) {
	return testPlatformConfig(), nil
}
func (r *reportRuntime) ValidateSpace(string) error { return nil }
func (r *reportRuntime) ListApps(context.Context) ([]runtime.AppRef, error) {
	var refs []runtime.AppRef
	for ref := range r.apps {
		refs = append(refs, ref)
	}
	for ref := range r.broken {
		refs = append(refs, ref)
	}
	return refs, nil
}
func (r *reportRuntime) Status(_ context.Context, space, app string) (*runtime.AppStatus, error) {
	r.statusReads.Add(1)
	ref := runtime.AppRef{Space: space, App: app}
	if r.broken[ref] {
		return nil, errors.New("the runtime is not answering")
	}
	status := r.apps[ref]
	return &status, nil
}
func (r *reportRuntime) ListSpaces(context.Context) ([]string, error) {
	var names []string
	for name := range r.spaces {
		names = append(names, name)
	}
	return names, nil
}
func (r *reportRuntime) SpaceConfig(_ context.Context, space string) (config.SpaceConfig, error) {
	settings, ok := r.spaces[space]
	if !ok {
		return config.SpaceConfig{}, runtime.ErrSpaceNotFound
	}
	return settings, nil
}

type reportFixture struct {
	server  *Server
	runtime *reportRuntime
	handler http.Handler
}

func vuln(id, severity, pkg, installed, fixed string) build.Vulnerability {
	return build.Vulnerability{
		ID: id, Severity: severity, Package: pkg, Installed: installed, Fixed: fixed,
		Class: "os-pkgs", Type: "debian", Title: "Title of " + id, URL: "https://example.com/" + id,
		Description: "A long description nobody lists.",
	}
}

// newReportFixture is a small fleet with its records on a data volume:
//
//	shop/web     public     three vulnerabilities (one waived), one credential
//	shop/api     internal   the shared critical one, read as not affected
//	tools/wiki   protected  the shared one (high there), and a medium one
//	tools/legacy protected  never scanned, no build on record
func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	rt := &reportRuntime{
		apps: map[runtime.AppRef]runtime.AppStatus{
			{Space: "shop", App: "web"}:     {Found: true, Image: "registry/shop/web:1", Stack: "node", PublicHost: "web.shop.example.com"},
			{Space: "shop", App: "api"}:     {Found: true, Image: "registry/shop/api:1", Stack: "python"},
			{Space: "tools", App: "wiki"}:   {Found: true, Image: "registry/tools/wiki:1", Stack: "python", PublicHost: "wiki.tools.example.com", Protected: true},
			{Space: "tools", App: "legacy"}: {Found: true, Image: "registry/tools/legacy:1", PublicHost: "legacy.tools.example.com", Protected: true},
		},
		spaces: map[string]config.SpaceConfig{
			"shop": {
				Admins:     config.AccessConfig{Users: []string{"ada@shop.example"}, Groups: []string{"shop-admins"}},
				Developers: config.AccessConfig{Users: []string{"dev@shop.example"}},
				Viewers:    config.AccessConfig{Users: []string{"viewer@shop.example"}},
				SecurityWaivers: []config.SecurityWaiver{
					{App: "web", ID: "CVE-2026-0002", Reason: "=not reachable from outside", By: "ada@shop.example", Expires: "2099-01-01"},
					// Ran out: the finding is back to what the policy says.
					{App: "web", ID: "CVE-2026-0001", Reason: "accepted for the launch", By: "ada@shop.example", Expires: "2020-01-01"},
				},
			},
			"tools": {},
		},
	}
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey, DataDir: t.TempDir()}, rt: rt}
	ctx := context.Background()

	store := func(space, app string, report *build.ScanReport, os string, endOfLife bool, verdicts map[string]ai.Verdict) {
		t.Helper()
		record := scanRecord(report, defaultPolicy(), evaluateScan(report, defaultPolicy()), verdicts)
		record.OS, record.EndOfLife = os, endOfLife
		record.ScannedAt = "2026-11-10T08:00:00Z"
		record.SBOM = encodeSBOM([]byte(`{"spdxVersion":"SPDX-2.3"}`))
		if err := s.storeScan(ctx, space, app, rt.apps[runtime.AppRef{Space: space, App: app}].Image, record); err != nil {
			t.Fatal(err)
		}
	}
	shared := vuln("CVE-2026-0001", "critical", "openssl", "3.0.11", "3.0.13")
	shared.Score, shared.Published = 9.8, "2026-10-01T00:00:00Z"
	alsoShared := shared
	alsoShared.Package = "libssl3"
	lodash := vuln("CVE-2026-0002", "critical", "lodash", "4.17.20", "4.17.21")
	lodash.Class, lodash.Type, lodash.Path = "lang-pkgs", "node-pkg", "app/node_modules/lodash/package.json"
	store("shop", "web", &build.ScanReport{
		Vulnerabilities: []build.Vulnerability{shared, alsoShared, lodash, vuln("CVE-2026-0003", "low", "zlib1g", "1.2.13", "")},
		Secrets:         []build.Secret{{Target: "app/.env", Rule: "aws-access-key-id", Severity: "critical", Title: "AWS Access Key ID", Line: 3, Match: "AWS_KEY=****"}},
	}, "debian 12.5", false, nil)
	store("shop", "api", &build.ScanReport{Vulnerabilities: []build.Vulnerability{shared}}, "debian 12.7", false, map[string]ai.Verdict{
		"CVE-2026-0001": {ID: "CVE-2026-0001", Verdict: ai.VerdictNotAffected, Confidence: ai.ConfidenceHigh, Reason: "The app never opens a TLS connection."},
	})
	elsewhere := vuln("CVE-2026-0001", "high", "openssl", "3.1.4", "3.1.5")
	elsewhere.Type, elsewhere.Published = "alpine", "2026-09-15T00:00:00Z"
	store("tools", "wiki", &build.ScanReport{
		Vulnerabilities: []build.Vulnerability{elsewhere, vuln("CVE-2026-0004", "medium", "busybox", "1.36", "1.37")},
	}, "alpine 3.16.2", true, nil)

	release := func(space, app string, ok bool, found PlanBuild, baseImages []string) {
		t.Helper()
		record := AppRelease{Plan: &Plan{Build: found}, Update: DeployUpdate{BaseImages: baseImages}, Result: DeployResult{OK: ok}}
		raw, err := marshalRelease(record)
		if err != nil {
			t.Fatal(err)
		}
		image := rt.apps[runtime.AppRef{Space: space, App: app}].Image
		if _, err := s.deploymentStore().RecordBuild(ctx, space, app, image, ok, raw); err != nil {
			t.Fatal(err)
		}
	}
	release("shop", "web", true, PlanBuild{
		Runtimes:        []PlanSignal{{Value: "Node.js", Evidence: "package.json"}},
		Frameworks:      []PlanSignal{{Value: "Next.js", Evidence: "package.json depends on next"}, {Value: "Prisma (needs `prisma generate` during the build)", Evidence: "package.json depends on @prisma/client"}},
		Versions:        []PlanSignal{{Value: "node >=18", Evidence: "package.json > engines.node"}},
		PackageManagers: []PlanSignal{{Value: "Node: pnpm@9.1.0 (declared, wins over lockfiles)", Evidence: "package.json > packageManager"}, {Value: "Node: engines.pnpm = 9", Evidence: "package.json > engines"}},
		BaseImages:      []string{"node:22"},
	}, []string{"docker.io/library/node:20.11.1-alpine@sha256:abc"})
	// A redeploy of the same image records a build that detected nothing;
	// the build that made the image is the one to read.
	release("shop", "web", true, PlanBuild{Summary: "Use the selected image without rebuilding it", Strategy: "existing-image"}, nil)
	release("shop", "api", true, PlanBuild{
		Runtimes:        []PlanSignal{{Value: "Python", Evidence: "pyproject.toml"}},
		Frameworks:      []PlanSignal{{Value: "FastAPI", Evidence: "a Python dependency file names fastapi"}, {Value: "uvicorn (a production ASGI server is already a dependency)"}},
		Versions:        []PlanSignal{{Value: "python >=3.11", Evidence: "pyproject.toml > requires-python"}},
		PackageManagers: []PlanSignal{{Value: "Python: uv", Evidence: "uv.lock"}},
	}, []string{"python:3.12-slim"})
	release("tools", "wiki", true, PlanBuild{
		Runtimes:   []PlanSignal{{Value: "Python", Evidence: "requirements.txt"}, {Value: "Node.js", Evidence: "package.json"}},
		Frameworks: []PlanSignal{{Value: "Django"}, {Value: "Django (manage.py present — collectstatic and migrations are usually needed)", Evidence: "manage.py"}},
		Versions:   []PlanSignal{{Value: "python 3.9", Evidence: ".python-version"}},
	}, []string{"python:3.12-slim", "scratch"})

	return &reportFixture{server: s, runtime: rt, handler: s.orchestratorRoutes()}
}

// get asks the orchestrator the way donkey-server forwards a request.
func (f *reportFixture) get(t *testing.T, path, email string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Access-Token", sealedToken(t, testSessionKey, email))
	req.Header.Set(orchestratorTokenHeader, orchestratorToken(testSessionKey))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *reportFixture) vulnerabilities(t *testing.T, query string) VulnerabilityReport {
	t.Helper()
	rec := f.get(t, "/v1/vulnerabilities"+query, "u1@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("vulnerabilities: %d %s", rec.Code, rec.Body.String())
	}
	var report VulnerabilityReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func findingIDs(findings []FindingGroup) []string {
	ids := []string{}
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	return ids
}

func affectedIn(t *testing.T, finding FindingGroup, space, app string) AffectedApp {
	t.Helper()
	for _, affected := range finding.Apps {
		if affected.Space == space && affected.App == app {
			return affected
		}
	}
	t.Fatalf("%s does not list %s/%s: %+v", finding.ID, space, app, finding.Apps)
	return AffectedApp{}
}

func TestVulnerabilitiesAreGroupedByFindingWithAppsAndContacts(t *testing.T) {
	f := newReportFixture(t)
	report := f.vulnerabilities(t, "")

	// Worst first, and among equals the one in the most apps.
	if got := strings.Join(findingIDs(report.Findings), " "); got != "CVE-2026-0001 CVE-2026-0002 aws-access-key-id CVE-2026-0004 CVE-2026-0003" {
		t.Fatalf("order: %s", got)
	}
	shared := report.Findings[0]
	if shared.Kind != FindingKindVulnerability || shared.Severity != "critical" || shared.Score != 9.8 || !shared.Fixable ||
		shared.Title != "Title of CVE-2026-0001" || shared.URL == "" || shared.Published != "2026-09-15T00:00:00Z" || len(shared.Apps) != 3 {
		t.Fatalf("shared finding: %+v", shared)
	}
	// One app, two packages of the same finding: one entry.
	web := affectedIn(t, shared, "shop", "web")
	if len(web.Packages) != 2 || web.Packages[0].Name != "libssl3" || web.Packages[1].Name != "openssl" ||
		web.Packages[1].Installed != "3.0.11" || web.Packages[1].Fixed != "3.0.13" || web.Packages[1].Type != "debian" {
		t.Fatalf("packages: %+v", web.Packages)
	}
	// Its waiver ran out, so the policy refuses it again.
	if web.State != FindingBlocking || web.Note != "" {
		t.Fatalf("an expired waiver still counts: %+v", web)
	}
	api := affectedIn(t, shared, "shop", "api")
	if api.State != FindingJudged || !strings.Contains(api.Note, "never opens a TLS connection") {
		t.Fatalf("the assistant's reading was lost: %+v", api)
	}
	// The same CVE weighs less in another distribution, below the threshold.
	wiki := affectedIn(t, shared, "tools", "wiki")
	if wiki.Severity != "high" || wiki.State != FindingFixable {
		t.Fatalf("wiki: %+v", wiki)
	}

	waived := affectedIn(t, report.Findings[1], "shop", "web")
	if waived.State != FindingWaived || waived.Note != "Waived by ada@shop.example until 2099-01-01: =not reachable from outside" ||
		waived.Packages[0].Path != "app/node_modules/lodash/package.json" {
		t.Fatalf("waived: %+v", waived)
	}
	secret := report.Findings[2]
	if secret.Kind != FindingKindSecret || secret.Title != "AWS Access Key ID" || secret.Severity != "critical" || secret.Fixable ||
		len(secret.Apps) != 1 || secret.Apps[0].State != FindingBlocking || secret.Apps[0].Locations[0] != "app/.env:3" {
		t.Fatalf("secret: %+v", secret)
	}
	if unfixed := report.Findings[4].Apps[0]; unfixed.State != FindingUnfixed || report.Findings[4].Fixable {
		t.Fatalf("unfixed: %+v", report.Findings[4])
	}

	// Who answers for the spaces: admins and developers, never viewers.
	shop := report.Spaces["shop"]
	if strings.Join(shop.Admins.Users, ",") != "ada@shop.example" || strings.Join(shop.Admins.Groups, ",") != "shop-admins" ||
		strings.Join(shop.Developers.Users, ",") != "dev@shop.example" {
		t.Fatalf("contacts: %+v", shop)
	}
	if tools, ok := report.Spaces["tools"]; !ok || !tools.Admins.Empty() {
		t.Fatalf("a space nobody was given: %+v %t", tools, ok)
	}
	if len(report.PlatformAdmins.Users) != 2 {
		t.Fatalf("platform admins: %+v", report.PlatformAdmins)
	}

	// Every running app is listed, so what the findings do not cover shows.
	if len(report.Apps) != 4 || !report.Scanning || report.Block != config.ScanBlockCritical {
		t.Fatalf("apps: %+v", report.Apps)
	}
	byName := map[string]ReportApp{}
	for _, app := range report.Apps {
		byName[app.Space+"/"+app.App] = app
	}
	if web := byName["shop/web"]; !web.Scanned || web.Exposure != "public" || web.URL != "https://web.shop.example.com" ||
		web.ScannedAt != "2026-11-10T08:00:00Z" || web.OS != "debian 12.5" || web.Image != "registry/shop/web:1" {
		t.Fatalf("web: %+v", web)
	}
	if api := byName["shop/api"]; api.Exposure != "internal" || api.URL != "" {
		t.Fatalf("api: %+v", api)
	}
	if wiki := byName["tools/wiki"]; wiki.Exposure != "protected" || !wiki.EndOfLife {
		t.Fatalf("wiki: %+v", wiki)
	}
	if legacy := byName["tools/legacy"]; legacy.Scanned || legacy.Problem != "" {
		t.Fatalf("legacy: %+v", legacy)
	}
	// Neither the inventories nor the prose travel with a report.
	if body := f.get(t, "/v1/vulnerabilities", "u1@example.com").Body.String(); strings.Contains(body, "sbom") ||
		strings.Contains(body, "A long description") || strings.Contains(body, "AWS_KEY") {
		t.Fatal("the report carries what it has no use for")
	}
}

func TestVulnerabilityFilters(t *testing.T) {
	f := newReportFixture(t)
	for _, tc := range []struct {
		query string
		want  string
		// apps is how many apps the first finding still lists.
		apps int
	}{
		{"?severity=critical", "CVE-2026-0001 CVE-2026-0002 aws-access-key-id", 3},
		{"?severity=medium,low", "CVE-2026-0004 CVE-2026-0003", 1},
		{"?severity=medium&severity=low", "CVE-2026-0004 CVE-2026-0003", 1},
		{"?fixable=true", "CVE-2026-0001 CVE-2026-0002 CVE-2026-0004", 3},
		// Naming the finding keeps it whole: by id, title or package.
		{"?q=cve-2026-0001", "CVE-2026-0001", 3},
		{"?q=LIBSSL", "CVE-2026-0001", 3},
		{"?q=access+key", "aws-access-key-id", 1},
		// Naming a space or an app keeps only what is in it.
		{"?q=tools", "CVE-2026-0001 CVE-2026-0004", 1},
		{"?q=shop/api", "CVE-2026-0001", 1},
		{"?q=wiki&severity=medium", "CVE-2026-0004", 1},
		{"?q=nothing-like-this", "", 0},
	} {
		report := f.vulnerabilities(t, tc.query)
		if got := strings.Join(findingIDs(report.Findings), " "); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.query, got, tc.want)
			continue
		}
		if len(report.Findings) > 0 && len(report.Findings[0].Apps) != tc.apps {
			t.Errorf("%s: first finding lists %d apps, want %d", tc.query, len(report.Findings[0].Apps), tc.apps)
		}
		// A filter narrows the findings, never what they were read from.
		if len(report.Apps) != 4 {
			t.Errorf("%s: %d apps", tc.query, len(report.Apps))
		}
	}
}

func readCSV(t *testing.T, rec *httptest.ResponseRecorder) (header []string, rows []map[string]string) {
	t.Helper()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\xEF\xBB\xBF") {
		t.Fatal("the CSV does not say it is UTF-8")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xEF\xBB\xBF"))).ReadAll()
	if err != nil || len(records) == 0 {
		t.Fatalf("csv: %v", err)
	}
	header = records[0]
	for _, record := range records[1:] {
		row := map[string]string{}
		for i, name := range header {
			row[name] = record[i]
		}
		rows = append(rows, row)
	}
	return header, rows
}

func TestVulnerabilitiesExportOneRowPerFindingAppAndPackage(t *testing.T) {
	f := newReportFixture(t)
	rec := f.get(t, "/v1/vulnerabilities?format=csv", "u1@example.com")
	header, rows := readCSV(t, rec)
	if disposition := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, `attachment; filename="donkey-vulnerabilities-`) ||
		!strings.HasSuffix(disposition, `.csv"`) {
		t.Fatalf("download name: %s", disposition)
	}
	if strings.Join(header, ",") != strings.Join(findingsCSVHeader, ",") {
		t.Fatalf("header: %v", header)
	}
	// shared: web twice (two packages), api, wiki; then lodash, the
	// credential, busybox and zlib.
	if len(rows) != 8 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	first := rows[0]
	if first["id"] != "CVE-2026-0001" || first["space"] != "shop" || first["app"] != "api" || first["state"] != FindingJudged ||
		first["package"] != "openssl" || first["installed"] != "3.0.11" || first["fixed"] != "3.0.13" || first["ecosystem"] != "debian" ||
		first["exposure"] != "internal" || first["score"] != "9.8" || first["scanned_at"] != "2026-11-10T08:00:00Z" ||
		first["reference"] != "https://example.com/CVE-2026-0001" {
		t.Fatalf("first row: %+v", first)
	}
	// The contacts ride on every row, groups marked as groups.
	if first["space_admins"] != "ada@shop.example; group:shop-admins" || first["space_developers"] != "dev@shop.example" {
		t.Fatalf("contacts: %+v", first)
	}
	var waived, secret map[string]string
	for _, row := range rows {
		switch row["id"] {
		case "CVE-2026-0002":
			waived = row
		case "aws-access-key-id":
			secret = row
		}
	}
	// A note is somebody's words; one that opens like a formula is made text.
	if waived["note"] != "Waived by ada@shop.example until 2099-01-01: =not reachable from outside" || waived["url"] != "https://web.shop.example.com" ||
		waived["location"] != "app/node_modules/lodash/package.json" {
		t.Fatalf("waived row: %+v", waived)
	}
	if secret["kind"] != FindingKindSecret || secret["location"] != "app/.env:3" || secret["package"] != "" || secret["severity"] != "critical" {
		t.Fatalf("secret row: %+v", secret)
	}

	// An export holds what the same filter shows.
	_, rows = readCSV(t, f.get(t, "/v1/vulnerabilities?format=csv&q=tools", "u1@example.com"))
	if len(rows) != 2 || rows[0]["app"] != "wiki" || rows[1]["app"] != "wiki" || rows[0]["space_admins"] != "" {
		t.Fatalf("filtered rows: %+v", rows)
	}
	if rec := f.get(t, "/v1/vulnerabilities?format=xlsx", "u1@example.com"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown format: %d", rec.Code)
	}
}

func TestCSVCellsCannotRunAsFormulas(t *testing.T) {
	for value, want := range map[string]string{
		"=HYPERLINK(\"https://evil.example\")": "'=HYPERLINK(\"https://evil.example\")",
		"+1+1":                                 "'+1+1",
		"-2+3":                                 "'-2+3",
		"@babel/core":                          "'@babel/core",
		"\tcmd":                                "'\tcmd",
		"openssl":                              "openssl",
		"3.0.11":                               "3.0.11",
		"two\nlines\r\nhere":                   "two lines here",
		"":                                     "",
	} {
		if got := csvCell(value); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", value, got, want)
		}
	}
}

func technologyNames(category TechnologyCategory) []string {
	names := []string{}
	for _, technology := range category.Technologies {
		names = append(names, technology.Name)
	}
	return names
}

func (f *reportFixture) technologies(t *testing.T, query string) map[string]TechnologyCategory {
	t.Helper()
	rec := f.get(t, "/v1/technologies"+query, "u1@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("technologies: %d %s", rec.Code, rec.Body.String())
	}
	var report TechnologyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Apps) != 4 || len(report.Spaces) != 2 {
		t.Fatalf("report: %+v", report)
	}
	categories := map[string]TechnologyCategory{}
	for _, category := range report.Categories {
		categories[category.ID] = category
	}
	return categories
}

func uses(t *testing.T, category TechnologyCategory, name string) Technology {
	t.Helper()
	for _, technology := range category.Technologies {
		if technology.Name == name {
			return technology
		}
	}
	t.Fatalf("%s is not listed in %s: %v", name, category.ID, technologyNames(category))
	return Technology{}
}

func TestTechnologiesAreListedMostUsedFirst(t *testing.T) {
	f := newReportFixture(t)
	categories := f.technologies(t, "")
	if len(categories) != len(technologyCategories) {
		t.Fatalf("categories: %+v", categories)
	}

	// Python runs in two apps, Node.js in one and is found in another's source.
	languages := categories[TechnologyLanguage]
	if got := strings.Join(technologyNames(languages), ","); got != "Node.js,Python" {
		t.Fatalf("languages: %s", got)
	}
	python := uses(t, languages, "Python")
	if python.Key != "python" || len(python.Apps) != 2 || python.Apps[0].App != "api" || python.Apps[1].App != "wiki" {
		t.Fatalf("python: %+v", python)
	}
	// The image an app runs on says its version; the version its source asks
	// for only stands in where no image does.
	if python.Apps[0].Version != "3.12" || python.Apps[0].Evidence != "python:3.12-slim" {
		t.Fatalf("python version: %+v", python.Apps[0])
	}
	node := uses(t, languages, "Node.js")
	if len(node.Apps) != 2 || node.Apps[0].App != "web" || node.Apps[0].Version != "20.11" || node.Apps[1].App != "wiki" || node.Apps[1].Version != "" {
		t.Fatalf("node: %+v", node)
	}

	// Names, not the advice the signals carry; a framework named twice once.
	frameworks := categories[TechnologyFramework]
	if got := strings.Join(technologyNames(frameworks), ","); got != "Django,FastAPI,Next.js,Prisma,uvicorn" {
		t.Fatalf("frameworks: %s", got)
	}
	if django := uses(t, frameworks, "Django"); len(django.Apps) != 1 {
		t.Fatalf("django: %+v", django)
	}

	// The images a build resolved, as two Dockerfiles would both write them.
	images := categories[TechnologyBaseImage]
	if got := strings.Join(technologyNames(images), ","); got != "python:3.12-slim,node:20.11.1-alpine" {
		t.Fatalf("base images: %s", got)
	}
	if len(uses(t, images, "python:3.12-slim").Apps) != 2 {
		t.Fatalf("base image usage: %+v", images)
	}

	// Operating systems by the release that is patched as one.
	systems := categories[TechnologyOS]
	if got := strings.Join(technologyNames(systems), ","); got != "debian 12,alpine 3.16" {
		t.Fatalf("operating systems: %s", got)
	}
	if debian := uses(t, systems, "debian 12"); len(debian.Apps) != 2 || debian.EndOfLife || debian.Apps[0].Version != "debian 12.7" {
		t.Fatalf("debian: %+v", debian)
	}
	if alpine := uses(t, systems, "alpine 3.16"); !alpine.EndOfLife {
		t.Fatalf("alpine: %+v", alpine)
	}

	// Tools, not constraints.
	managers := categories[TechnologyPackageManager]
	if got := strings.Join(technologyNames(managers), ","); got != "pnpm,uv" {
		t.Fatalf("package managers: %s", got)
	}
	if pnpm := uses(t, managers, "pnpm"); pnpm.Apps[0].Version != "9.1.0" {
		t.Fatalf("pnpm: %+v", pnpm)
	}
}

func TestTechnologyFiltersAndExport(t *testing.T) {
	f := newReportFixture(t)
	categories := f.technologies(t, "?category=language&q=wiki")
	if len(categories) != 1 {
		t.Fatalf("categories: %+v", categories)
	}
	// Naming an app keeps the technologies it uses, with that app alone.
	languages := categories[TechnologyLanguage]
	if got := strings.Join(technologyNames(languages), ","); got != "Node.js,Python" || len(languages.Technologies[1].Apps) != 1 {
		t.Fatalf("filtered languages: %+v", languages)
	}
	// Naming a technology keeps it whole.
	if python := uses(t, f.technologies(t, "?q=pyth")[TechnologyLanguage], "Python"); len(python.Apps) != 2 {
		t.Fatalf("python: %+v", python)
	}

	rec := f.get(t, "/v1/technologies?format=csv&category=os", "u1@example.com")
	header, rows := readCSV(t, rec)
	if strings.Join(header, ",") != strings.Join(technologiesCSVHeader, ",") || len(rows) != 3 {
		t.Fatalf("csv: %v %+v", header, rows)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["app"] < rows[j]["app"] })
	if api := rows[0]; api["category"] != "os" || api["technology"] != "debian 12" || api["version"] != "debian 12.7" ||
		api["space"] != "shop" || api["exposure"] != "internal" || api["space_admins"] != "ada@shop.example; group:shop-admins" || api["end_of_life"] != "" {
		t.Fatalf("api row: %+v", api)
	}
	if wiki := rows[2]; wiki["technology"] != "alpine 3.16" || wiki["end_of_life"] != "yes" || wiki["url"] != "https://wiki.tools.example.com" {
		t.Fatalf("wiki row: %+v", wiki)
	}
}

// The reports are the platform's: a member of every space still is not who
// they are for.
func TestReportsAreForPlatformAdmins(t *testing.T) {
	f := newReportFixture(t)
	for _, path := range []string{"/v1/vulnerabilities", "/v1/technologies", "/v1/vulnerabilities?format=csv"} {
		if rec := f.get(t, path, "ada@shop.example"); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a space admin: %d %s", path, rec.Code, rec.Body.String())
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", path, rec.Code)
		}
	}
}

func TestReportsReadRecordsOncePerImage(t *testing.T) {
	f := newReportFixture(t)
	ref := runtime.AppRef{Space: "shop", App: "api"}
	f.vulnerabilities(t, "")
	first := f.server.facts.get(ref)
	if first == nil || first.scan == nil || first.build == nil || first.scan.SBOM != "" {
		t.Fatalf("facts: %+v", first)
	}

	// The next report, past the inventory's moment, reads no record again.
	f.server.inventory.forget()
	f.vulnerabilities(t, "")
	if f.server.facts.get(ref) != first {
		t.Fatal("unchanged records were read again")
	}

	// A rescan rewrites the scan: the report reads it, and keeps the build.
	fresh := &build.ScanReport{Vulnerabilities: []build.Vulnerability{vuln("CVE-2026-0009", "critical", "curl", "8.0", "8.1")}}
	record := scanRecord(fresh, defaultPolicy(), evaluateScan(fresh, defaultPolicy()), nil)
	if err := f.server.storeScan(context.Background(), "shop", "api", "registry/shop/api:1", record); err != nil {
		t.Fatal(err)
	}
	report := f.vulnerabilities(t, "")
	if got := strings.Join(findingIDs(report.Findings), " "); !strings.Contains(got, "CVE-2026-0009") {
		t.Fatalf("a refreshed scan did not reach the report: %s", got)
	}
	if again := f.server.facts.get(ref); again == first || again.build == nil {
		t.Fatalf("facts after a rescan: %+v", again)
	}

	// A new image is a different app as far as the records go.
	status := f.runtime.apps[ref]
	status.Image = "registry/shop/api:2"
	f.runtime.apps[ref] = status
	f.server.forgetFacts("shop", "api")
	report = f.vulnerabilities(t, "")
	for _, app := range report.Apps {
		if app.Space == "shop" && app.App == "api" && (app.Scanned || app.Image != "registry/shop/api:2") {
			t.Fatalf("the old image's scan was shown for the new one: %+v", app)
		}
	}

	// An app that is gone takes its facts with it.
	delete(f.runtime.apps, ref)
	f.server.inventory.forget()
	f.vulnerabilities(t, "")
	if f.server.facts.get(ref) != nil {
		t.Fatal("a removed app's facts were kept")
	}
}

// One app the runtime cannot describe is listed as such; the report of
// every other app still comes.
func TestReportsSurviveAnAppThatCannotBeRead(t *testing.T) {
	f := newReportFixture(t)
	f.runtime.broken = map[runtime.AppRef]bool{{Space: "tools", App: "ghost"}: true}
	report := f.vulnerabilities(t, "")
	if len(report.Apps) != 5 || len(report.Findings) != 5 {
		t.Fatalf("report: %d apps, %d findings", len(report.Apps), len(report.Findings))
	}
	for _, app := range report.Apps {
		if app.App == "ghost" && (app.Problem == "" || app.Scanned) {
			t.Fatalf("ghost: %+v", app)
		}
	}
}

func TestReportInventoryIsSharedForAMoment(t *testing.T) {
	f := newReportFixture(t)
	f.vulnerabilities(t, "")
	reads := f.runtime.statusReads.Load()
	// The export of what is on screen, and the other report, ask again.
	f.get(t, "/v1/vulnerabilities?format=csv", "u1@example.com")
	f.technologies(t, "")
	if again := f.runtime.statusReads.Load(); again != reads {
		t.Fatalf("each report read the fleet again: %d then %d", reads, again)
	}
	// Whoever writes a record is not held up by a report being read, and
	// the next report reads again.
	f.server.forgetFacts("shop", "web")
	f.vulnerabilities(t, "")
	if f.runtime.statusReads.Load() == reads {
		t.Fatal("a changed record did not reach the next report")
	}
}

func TestBrieflyDoesNotKeepAnAnswerReadWhileSomethingChanged(t *testing.T) {
	var cache briefly[int]
	reads := 0
	read := func() (int, error) { reads++; return reads, nil }
	if v, _ := cache.get(time.Minute, read); v != 1 {
		t.Fatal(v)
	}
	if v, _ := cache.get(time.Minute, read); v != 1 || reads != 1 {
		t.Fatalf("read again within the moment: %d", reads)
	}
	// A change during a read: the answer is handed out, but not kept.
	cache.forget()
	if v, _ := cache.get(time.Minute, func() (int, error) { cache.forget(); return 7, nil }); v != 7 {
		t.Fatal(v)
	}
	if v, _ := cache.get(time.Minute, read); v != 2 {
		t.Fatalf("an answer read during a change was kept: %d", v)
	}
	// A failure is not kept for the moment a good answer is.
	failing := errors.New("no answer")
	cache.forget()
	if _, err := cache.get(time.Minute, func() (int, error) { return 0, failing }); !errors.Is(err, failing) {
		t.Fatal(err)
	}
	time.Sleep(failedCacheTTL + 50*time.Millisecond)
	if v, err := cache.get(time.Minute, read); err != nil || v != 3 {
		t.Fatalf("a failure was kept: %d %v", v, err)
	}
}

// A report repeats itself for every app a finding is in; a client that takes
// compression gets it, and one that does not gets the plain body.
func TestReportsAreCompressedForAClientThatTakesIt(t *testing.T) {
	f := newReportFixture(t)
	ask := func(path, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Access-Token", sealedToken(t, testSessionKey, "u1@example.com"))
		req.Header.Set(orchestratorTokenHeader, orchestratorToken(testSessionKey))
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec
	}
	unpack := func(rec *httptest.ResponseRecorder) string {
		if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("not compressed: %+v", rec.Header())
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	plain := ask("/v1/vulnerabilities", "")
	if plain.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(plain.Body.String(), "{") {
		t.Fatalf("a client that did not ask got something else: %+v", plain.Header())
	}
	packed := ask("/v1/vulnerabilities", "br;q=1.0, gzip;q=0.8, *;q=0.1")
	if body := unpack(packed); body != plain.Body.String() || packed.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("the compressed report differs from the plain one")
	}
	if sheet := unpack(ask("/v1/technologies?format=csv", "gzip")); !strings.HasPrefix(sheet, "\xEF\xBB\xBFcategory,technology,") {
		t.Fatalf("compressed CSV: %q", sheet[:min(len(sheet), 60)])
	}
	// "gzip" has to be an encoding the client names, not a part of one.
	if rec := ask("/v1/technologies", "x-gzip-like, identity"); rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("compressed for a client that never asked for gzip")
	}
}

// A site served as files is built on a web server's image: that names the
// stack, but its tag is the server's version, not a language's.
func TestStaticSiteHasNoLanguageVersion(t *testing.T) {
	s := &Server{cfg: &Config{Domain: "example.com"}}
	inventory := &fleetInventory{At: time.Now(), Apps: []inventoryApp{
		{
			AppRef: runtime.AppRef{Space: "tools", App: "docs"},
			Status: runtime.AppStatus{Found: true, Stack: "static"},
			Facts:  &appFacts{build: &PlanBuild{BaseImages: []string{"caddy:2-alpine"}}},
		},
		{
			// Built in one image, run on another: the first of a language wins.
			AppRef: runtime.AppRef{Space: "tools", App: "site"},
			Status: runtime.AppStatus{Found: true, Stack: "node"},
			Facts:  &appFacts{build: &PlanBuild{BaseImages: []string{"node:22-alpine", "nginx:1.27", "node:20"}}},
		},
		// Nothing on record at all: listed as an app, used by nothing.
		{AppRef: runtime.AppRef{Space: "tools", App: "old"}, Status: runtime.AppStatus{Found: true}},
	}}
	report := s.technologyReport(context.Background(), inventory)
	if len(report.Apps) != 3 {
		t.Fatalf("apps: %+v", report.Apps)
	}
	languages := report.Categories[0]
	static := uses(t, languages, "Static site")
	if static.Key != "static" || len(static.Apps) != 1 || static.Apps[0].Version != "" || static.Apps[0].Evidence != "caddy:2-alpine" {
		t.Fatalf("static site: %+v", static)
	}
	if node := uses(t, languages, "Node.js"); node.Apps[0].Version != "22" || node.Apps[0].Evidence != "node:22-alpine" {
		t.Fatalf("node: %+v", node)
	}
	if images := technologyNames(report.Categories[2]); strings.Join(images, ",") != "caddy:2-alpine,nginx:1.27,node:20,node:22-alpine" {
		t.Fatalf("base images: %v", images)
	}
}
