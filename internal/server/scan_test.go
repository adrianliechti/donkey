package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

type unavailableScanPolicy struct{ repairRuntime }

func (unavailableScanPolicy) SpaceConfig(context.Context, string) (config.SpaceConfig, error) {
	return config.SpaceConfig{}, errors.New("runtime unavailable")
}

func TestScanPolicyFailureIsNotARefusal(t *testing.T) {
	s := &Server{cfg: &Config{}, rt: unavailableScanPolicy{}}
	out := newLineStreamer(httptest.NewRecorder(), nil)
	err := s.scanJudge(out, &config.AppConfig{Name: "app"}, "", "", "", nil)(context.Background(), fullReport())
	var scanErr *build.ScanError
	if !errors.As(err, &scanErr) || out.lastScan != nil {
		t.Fatalf("policy outage became a verdict: scan=%+v err=%v", out.lastScan, err)
	}
}

func TestSecurityBadgeKeepsNonblockingWarnings(t *testing.T) {
	policy := defaultPolicy()
	policy.Secrets = config.ScanSecretsWarn
	for _, report := range []*build.ScanReport{
		{Secrets: []build.Secret{awsKey}},
		{EndOfLife: true},
	} {
		record := scanRecord(report, policy, evaluateScan(report, policy), nil)
		if record.Blocked || securityOf(record).State != SecurityWarning {
			t.Fatalf("nonblocking warning misreported: %+v", record)
		}
	}
}

func TestStoredScanLookupFailuresDoNotBypassPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"history unavailable", http.StatusServiceUnavailable, "unavailable", true},
		{"unreadable record", http.StatusOK, "{", true},
		{"legacy image", http.StatusNotFound, "no record", false},
		{"legacy record", http.StatusOK, `{ "result": { "ok": true } }`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			s := &Server{cfg: &Config{}, telemetry: telemetry.NewClient(upstream.URL, nil)}
			response := httptest.NewRecorder()
			_, err := s.checkStoredScan(context.Background(), newLineStreamer(response, nil), "team", &config.AppConfig{Name: "app"}, "registry/app:old")
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if tc.wantError && strings.Contains(response.Body.String(), "deployed as it is") {
				t.Fatal("lookup failure presented as an unscanned legacy image")
			}
		})
	}
}

func TestStoredScanReevaluatesWithoutLosingEvidence(t *testing.T) {
	store, client := testBuildStore(t)
	policy := defaultPolicy()
	report := &build.ScanReport{Vulnerabilities: []build.Vulnerability{criticalFixable}, SBOM: []byte("inventory")}
	verdict := ai.Verdict{ID: criticalFixable.ID, Verdict: ai.VerdictNotAffected, Confidence: ai.ConfidenceHigh}
	verdicts := map[string]ai.Verdict{verdict.ID: verdict}
	record := scanRecord(report, policy, evaluateScan(report, policy), verdicts)
	record.ScannedAt = "2026-10-25T07:00:00Z"
	for _, block := range []string{config.ScanBlockNone, config.ScanBlockCritical} {
		raw, err := json.Marshal(AppRelease{Scan: record})
		if err != nil {
			t.Fatal(err)
		}
		store.stored["team/app"] = raw
		s := &Server{cfg: &Config{}, rt: repairRuntime{}, telemetry: client}
		s.cfg.Build.Scan.Block = block
		out := newLineStreamer(httptest.NewRecorder(), nil)
		record, err = s.checkStoredScan(context.Background(), out, "team", &config.AppConfig{Name: "app"}, "registry/app:old")
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Verdicts) != 1 || record.SBOM != encodeSBOM(report.SBOM) || record.ScannedAt != "2026-10-25T07:00:00Z" {
			t.Fatalf("policy re-evaluation lost evidence: %+v", record)
		}
	}
	if record.Summary.Judged != 1 || record.Blocked {
		t.Fatal("restoring the threshold lost the assistant's verdict")
	}
}

func TestScanAndBuildProgressStreamConcurrently(t *testing.T) {
	response := httptest.NewRecorder()
	out := newLineStreamer(response, newReleaseRecorder(time.Now(), nil))
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 100 {
			_, _ = out.Write([]byte("build progress\n"))
		}
	}()
	go func() {
		defer workers.Done()
		for range 100 {
			out.Line("scan finding")
			out.Scan(&AppScan{})
		}
	}()
	workers.Wait()
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 300 {
		t.Fatalf("stream has %d messages", len(lines))
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("corrupt progress event: %s", line)
		}
	}
}

var (
	criticalFixable = build.Vulnerability{ID: "CVE-2019-10744", Severity: "critical", Package: "lodash", Installed: "4.17.11", Fixed: "4.17.12", Class: "lang-pkgs", Type: "node-pkg", Path: "app/node_modules/lodash/package.json", Title: "prototype pollution"}
	criticalUnfixed = build.Vulnerability{ID: "CVE-2024-0001", Severity: "critical", Package: "musl", Installed: "1.2.4-r2", Status: "affected", Class: "os-pkgs", Type: "alpine"}
	highFixable     = build.Vulnerability{ID: "CVE-2023-5363", Severity: "high", Package: "libcrypto3", Installed: "3.1.0-r4", Fixed: "3.1.4-r0", Class: "os-pkgs", Type: "alpine"}
	mediumFixable   = build.Vulnerability{ID: "CVE-2024-0002", Severity: "medium", Package: "zlib", Installed: "1.2.13-r0", Fixed: "1.2.13-r1", Class: "os-pkgs", Type: "alpine"}
	awsKey          = build.Secret{Target: "app/.env", Rule: "aws-access-key-id", Title: "AWS Access Key ID", Severity: "critical", Line: 2, Match: "AWS_ACCESS_KEY_ID=****"}
)

func fullReport() *build.ScanReport {
	return &build.ScanReport{
		Scanner: "aquasec/trivy:test", OS: "alpine 3.20.10", DBUpdatedAt: "2026-10-25T06:00:00Z",
		Vulnerabilities: []build.Vulnerability{mediumFixable, criticalUnfixed, highFixable, criticalFixable},
		Secrets:         []build.Secret{awsKey},
	}
}

func defaultPolicy() ScanPolicy {
	return ScanPolicy{Block: config.ScanBlockCritical, IgnoreUnfixed: true, Secrets: config.ScanSecretsBlock, Triage: true}
}

func ids(vulns []build.Vulnerability) []string {
	out := []string{}
	for _, v := range vulns {
		out = append(out, v.ID)
	}
	return out
}

func findingCodes(fs []Finding) string {
	codes := []string{}
	for _, f := range fs {
		codes = append(codes, f.Level+":"+f.Code)
	}
	return strings.Join(codes, " ")
}

// The default policy: a critical with a fix refuses, a critical without one
// only informs, everything fixable below the threshold warns once, and a
// credential in the image refuses.
func TestEvaluateScanDefaultPolicy(t *testing.T) {
	eval := evaluateScan(fullReport(), defaultPolicy())
	if got := ids(eval.Blocking); strings.Join(got, ",") != "CVE-2019-10744" {
		t.Errorf("blocking = %v", got)
	}
	if got := ids(eval.Unfixed); strings.Join(got, ",") != "CVE-2024-0001" {
		t.Errorf("unfixed = %v", got)
	}
	// Worst first.
	if got := ids(eval.Fixable); strings.Join(got, ",") != "CVE-2023-5363,CVE-2024-0002" {
		t.Errorf("fixable = %v", got)
	}
	if len(eval.BlockingSecrets) != 1 || len(eval.WarnSecrets) != 0 {
		t.Errorf("secrets: blocking %d warn %d", len(eval.BlockingSecrets), len(eval.WarnSecrets))
	}

	record := scanRecord(fullReport(), defaultPolicy(), eval, nil)
	if !record.Blocked {
		t.Fatal("a fixable critical must refuse the image")
	}
	if got := findingCodes(record.Findings); got != "block:vuln-blocking block:secret-in-image warn:vuln-fixable info:vuln-unfixed" {
		t.Errorf("findings = %s", got)
	}
	first := record.Findings[0]
	if first.File != criticalFixable.Path || !strings.Contains(first.Title, "lodash 4.17.11") || !strings.Contains(first.Title, "fixed in 4.17.12") || !strings.Contains(first.Fix, "4.17.12") {
		t.Errorf("blocking finding = %+v", first)
	}
	if !strings.Contains(record.Findings[2].Title, "2 vulnerabilities below the critical threshold have a fix") {
		t.Errorf("fixable finding = %q", record.Findings[2].Title)
	}
	want := ScanSummary{Critical: 2, High: 1, Medium: 1, Fixable: 3, Unfixed: 1, Secrets: 1, Blocking: 2}
	if record.Summary != want {
		t.Errorf("summary = %+v, want %+v", record.Summary, want)
	}
	if got := record.Summary.Text(); got != "2 critical, 1 high, 1 medium; 3 with a fix; 1 secret" {
		t.Errorf("summary text = %q", got)
	}
	msg := refusalMessage(record)
	if !strings.HasPrefix(msg, "the image was refused by the vulnerability policy: CVE-2019-10744: lodash 4.17.11 has a critical vulnerability, fixed in 4.17.12, and 1 more.") || !strings.Contains(msg, "waiver") {
		t.Errorf("refusal = %q", msg)
	}
}

func TestEvaluateScanThresholds(t *testing.T) {
	high := defaultPolicy()
	high.Block = config.ScanBlockHigh
	if got := ids(evaluateScan(fullReport(), high).Blocking); strings.Join(got, ",") != "CVE-2019-10744,CVE-2023-5363" {
		t.Errorf("block: high → blocking = %v", got)
	}

	none := defaultPolicy()
	none.Block = config.ScanBlockNone
	none.Secrets = config.ScanSecretsWarn
	eval := evaluateScan(fullReport(), none)
	if len(eval.Blocking) != 0 || len(eval.Fixable) != 3 {
		t.Errorf("block: none → blocking %v fixable %v", ids(eval.Blocking), ids(eval.Fixable))
	}
	if record := scanRecord(fullReport(), none, eval, nil); record.Blocked {
		t.Error("block: none with secrets: warn must not refuse")
	} else if got := findingCodes(record.Findings); got != "warn:secret-in-image warn:vuln-fixable info:vuln-unfixed" {
		t.Errorf("findings = %s", got)
	}

	strict := defaultPolicy()
	strict.IgnoreUnfixed = false
	if got := ids(evaluateScan(fullReport(), strict).Blocking); strings.Join(got, ",") != "CVE-2019-10744,CVE-2024-0001" {
		t.Errorf("ignoreUnfixed: false → blocking = %v", got)
	}
}

// A space admin's waiver lifts one finding, by id for a vulnerability and by
// rule (or rule@file) for a secret, and the record says who and until when.
func TestEvaluateScanWaivers(t *testing.T) {
	policy := defaultPolicy()
	policy.Waivers = []config.SecurityWaiver{
		{App: "shop", ID: "cve-2019-10744", Reason: "defaultsDeep is never called", By: "ada@example.com", Expires: "2026-11-24"},
		{App: "shop", ID: "aws-access-key-id@app/.env", Reason: "a fixture key for the test suite", By: "ada@example.com"},
	}
	eval := evaluateScan(fullReport(), policy)
	if len(eval.Blocking) != 0 || len(eval.Waived) != 1 || len(eval.BlockingSecrets) != 0 || len(eval.WaivedSecrets) != 1 {
		t.Fatalf("eval = blocking %v waived %d blockingSecrets %d waivedSecrets %d", ids(eval.Blocking), len(eval.Waived), len(eval.BlockingSecrets), len(eval.WaivedSecrets))
	}
	record := scanRecord(fullReport(), policy, eval, nil)
	if record.Blocked {
		t.Fatal("everything blocking was waived")
	}
	if got := findingCodes(record.Findings); got != "warn:vuln-waived warn:secret-in-image warn:vuln-fixable info:vuln-unfixed" {
		t.Errorf("findings = %s", got)
	}
	if f := record.Findings[0]; !strings.Contains(f.Title, "waived by ada@example.com until 2026-11-24") || f.Detail != "defaultsDeep is never called" {
		t.Errorf("waived finding = %+v", f)
	}
	if record.Summary.Waived != 2 || record.Summary.Blocking != 2 {
		t.Errorf("summary = %+v", record.Summary)
	}

	// A waiver on another file's secret does not cover this one.
	policy.Waivers[1].ID = "aws-access-key-id@other/.env"
	if eval := evaluateScan(fullReport(), policy); len(eval.BlockingSecrets) != 1 {
		t.Error("a waiver scoped to another file must not apply")
	}
}

// The assistant's verdict lets a refused image through only as a
// high-confidence "not affected", and only while the policy allows it;
// every other verdict leaves the refusal and is shown beside it.
func TestScanRecordVerdicts(t *testing.T) {
	policy := defaultPolicy()
	policy.Secrets = config.ScanSecretsOff
	report := fullReport()
	report.Secrets = nil
	eval := evaluateScan(report, policy)

	waiving := ai.Verdict{ID: "CVE-2019-10744", Verdict: ai.VerdictNotAffected, Confidence: ai.ConfidenceHigh, Reason: "lodash is only required by the build script.", Evidence: []string{"package.json:12"}, Fix: "Move lodash to devDependencies."}
	record := scanRecord(report, policy, eval, map[string]ai.Verdict{waiving.ID: waiving})
	if record.Blocked {
		t.Fatal("a high-confidence not-affected verdict must waive the block")
	}
	if got := findingCodes(record.Findings); !strings.HasPrefix(got, "warn:vuln-judged") {
		t.Errorf("findings = %s", got)
	}
	if f := record.Findings[0]; !strings.Contains(f.Detail, "not-affected, high confidence") || !strings.Contains(f.Detail, "package.json:12") || f.Fix != "Move lodash to devDependencies." {
		t.Errorf("judged finding = %+v", f)
	}
	if record.Summary.Judged != 1 || len(record.Verdicts) != 1 {
		t.Errorf("summary = %+v, verdicts = %d", record.Summary, len(record.Verdicts))
	}

	for name, verdict := range map[string]ai.Verdict{
		"unsure":            {ID: waiving.ID, Verdict: ai.VerdictUnsure, Confidence: ai.ConfidenceHigh, Reason: "could not tell"},
		"affected":          {ID: waiving.ID, Verdict: ai.VerdictAffected, Confidence: ai.ConfidenceHigh, Reason: "used on every request"},
		"medium confidence": {ID: waiving.ID, Verdict: ai.VerdictNotAffected, Confidence: ai.ConfidenceMedium, Reason: "probably unused"},
	} {
		record := scanRecord(report, policy, eval, map[string]ai.Verdict{verdict.ID: verdict})
		if !record.Blocked {
			t.Errorf("%s: must still refuse", name)
		}
		if f := record.Findings[0]; f.Code != FindingVulnBlocking || !strings.Contains(f.Detail, verdict.Reason) {
			t.Errorf("%s: the verdict should be shown with the refusal: %+v", name, f)
		}
	}

	policy.Triage = false
	if record := scanRecord(report, policy, eval, map[string]ai.Verdict{waiving.ID: waiving}); !record.Blocked {
		t.Error("triage: off must leave the refusal in place whatever the verdict says")
	}
}

func TestScanRecordCleanAndEndOfLife(t *testing.T) {
	clean := &build.ScanReport{DBUpdatedAt: "2026-10-25T06:00:00Z", Vulnerabilities: []build.Vulnerability{}, Secrets: []build.Secret{}}
	record := scanRecord(clean, defaultPolicy(), evaluateScan(clean, defaultPolicy()), nil)
	if record.Blocked || findingCodes(record.Findings) != "info:vuln-clean" || !strings.Contains(record.Findings[0].Title, "2026-10-25") {
		t.Errorf("clean record = %+v", record.Findings)
	}
	if got := record.Summary.Text(); got != "no known vulnerabilities" {
		t.Errorf("summary text = %q", got)
	}

	eol := &build.ScanReport{OS: "alpine 3.17.0", EndOfLife: true, Vulnerabilities: []build.Vulnerability{criticalUnfixed}}
	record = scanRecord(eol, defaultPolicy(), evaluateScan(eol, defaultPolicy()), nil)
	if record.Blocked || findingCodes(record.Findings) != "info:vuln-unfixed warn:os-end-of-life" {
		t.Errorf("end-of-life record = %s", findingCodes(record.Findings))
	}
}

func TestScanRecordKeepsTheInventory(t *testing.T) {
	report := fullReport()
	report.SBOM = []byte(`{"bomFormat":"CycloneDX","components":[{"name":"lodash","version":"4.17.11"}]}`)
	record := scanRecord(report, defaultPolicy(), evaluateScan(report, defaultPolicy()), nil)
	if record.SBOM == "" || strings.Contains(record.SBOM, "lodash") {
		t.Fatalf("the inventory should be stored packed, got %q", record.SBOM)
	}
	back, err := record.SBOMBytes()
	if err != nil || string(back) != string(report.SBOM) {
		t.Fatalf("round trip = %q, %v", back, err)
	}
	if empty, err := (&AppScan{}).SBOMBytes(); empty != nil || err != nil {
		t.Errorf("no inventory = %q, %v", empty, err)
	}
}

func TestScanRecordShownLeavesTheInventoryOut(t *testing.T) {
	report := fullReport()
	report.SBOM = []byte(`{"spdxVersion":"SPDX-2.3"}`)
	record := scanRecord(report, defaultPolicy(), evaluateScan(report, defaultPolicy()), nil)
	shown := record.Shown()
	if shown.SBOM != "" || record.SBOM == "" {
		t.Errorf("the stream copy must drop the inventory and leave the record's: %q / %q", shown.SBOM, record.SBOM)
	}
	if shown.Summary != record.Summary || len(shown.Findings) != len(record.Findings) || len(shown.Vulnerabilities) != len(record.Vulnerabilities) {
		t.Error("the stream copy must say everything else")
	}
	if (*AppScan)(nil).Shown() != nil {
		t.Error("no record, nothing shown")
	}
}

// The badge a running app carries: one word for the fleet, the numbers
// behind it, and nothing for an image that was never scanned.
func TestSecurityOf(t *testing.T) {
	policy := defaultPolicy()
	policy.Secrets = config.ScanSecretsOff
	report := fullReport()
	report.Secrets = nil
	record := scanRecord(report, policy, evaluateScan(report, policy), nil)
	if got := securityOf(record).State; got != SecurityNeedsAction {
		t.Errorf("a refused image = %q", got)
	}
	waiving := ai.Verdict{ID: "CVE-2019-10744", Verdict: ai.VerdictNotAffected, Confidence: ai.ConfidenceHigh}
	record = scanRecord(report, policy, evaluateScan(report, policy), map[string]ai.Verdict{waiving.ID: waiving})
	if got := securityOf(record).State; got != SecurityWaived {
		t.Errorf("a judged image = %q", got)
	}
	below := &build.ScanReport{Vulnerabilities: []build.Vulnerability{highFixable, criticalUnfixed}}
	record = scanRecord(below, policy, evaluateScan(below, policy), nil)
	if got := securityOf(record).State; got != SecurityFixable {
		t.Errorf("fixes below the threshold = %q", got)
	}
	clean := &build.ScanReport{Vulnerabilities: []build.Vulnerability{criticalUnfixed}, DBUpdatedAt: "2026-10-25T06:00:00Z"}
	record = scanRecord(clean, policy, evaluateScan(clean, policy), nil)
	record.ScannedAt = "2026-10-25T07:00:00Z"
	security := securityOf(record)
	if security.State != SecurityClean || security.DBUpdatedAt != clean.DBUpdatedAt || security.ScannedAt != record.ScannedAt {
		t.Errorf("unfixed only = %+v", security)
	}
	back := parseSecurity(securityJSON(record))
	if back == nil || back.State != SecurityClean || back.Summary != record.Summary {
		t.Errorf("round trip = %+v", back)
	}
	if securityJSON(nil) != "" || parseSecurity("") != nil || parseSecurity("{}") != nil || parseSecurity("nonsense") != nil {
		t.Error("no scan, no badge")
	}
}
