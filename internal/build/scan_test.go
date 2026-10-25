package build

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const trivyFixture = `{
  "SchemaVersion": 2,
  "ArtifactName": "/target",
  "ArtifactType": "filesystem",
  "Metadata": {"OS": {"Family": "alpine", "Name": "3.18.0", "EOSL": true}},
  "Results": [
    {
      "Target": "/target (alpine 3.18.0)",
      "Class": "os-pkgs",
      "Type": "alpine",
      "Vulnerabilities": [
        {
          "VulnerabilityID": "CVE-2023-5363",
          "PkgName": "libcrypto3",
          "InstalledVersion": "3.1.0-r4",
          "FixedVersion": "3.1.4-r0",
          "Status": "fixed",
          "Severity": "HIGH",
          "Title": "openssl: Incorrect cipher key and IV length processing",
          "Description": "Issue summary: A bug has been identified in the processing of key and initialisation vector (IV) lengths.",
          "PrimaryURL": "https://avd.aquasec.com/nvd/cve-2023-5363",
          "PublishedDate": "2023-10-25T18:17:43.613Z",
          "CVSS": {"nvd": {"V3Score": 7.5}, "redhat": {"V3Score": 7.5}}
        },
        {
          "VulnerabilityID": "CVE-2024-0001",
          "PkgName": "busybox",
          "InstalledVersion": "1.36.0-r9",
          "Status": "affected",
          "Severity": "LOW",
          "CVSS": {"ghsa": {"V40Score": 3.1}}
        }
      ]
    },
    {
      "Target": "Node.js",
      "Class": "lang-pkgs",
      "Type": "node-pkg",
      "Vulnerabilities": [
        {
          "VulnerabilityID": "CVE-2019-10744",
          "PkgName": "lodash",
          "PkgPath": "/target/app/node_modules/lodash/package.json",
          "InstalledVersion": "4.17.11",
          "FixedVersion": "4.17.12",
          "Status": "fixed",
          "Severity": "CRITICAL",
          "Title": "nodejs-lodash: prototype pollution in defaultsDeep function leading to modifying properties",
          "PrimaryURL": "https://avd.aquasec.com/nvd/cve-2019-10744",
          "CVSS": {"nvd": {"V3Score": 9.1}}
        }
      ]
    },
    {
      "Target": "/target/app/.env",
      "Class": "secret",
      "Secrets": [
        {"RuleID": "aws-access-key-id", "Category": "AWS", "Severity": "CRITICAL", "Title": "AWS Access Key ID", "StartLine": 2, "Match": "AWS_ACCESS_KEY_ID=****************GHIJ"}
      ]
    }
  ]
}`

// The scanner's report is reduced to what donkey acts on: every finding
// keeps its identity, package, versions and fix; targets lose the mount
// prefix; severities are normalized.
func TestParseTrivyReport(t *testing.T) {
	report, err := parseTrivyReport([]byte(trivyFixture))
	if err != nil {
		t.Fatal(err)
	}
	if report.OS != "alpine 3.18.0" || !report.EndOfLife {
		t.Errorf("OS = %q end-of-life %v, want alpine 3.18.0 end-of-life", report.OS, report.EndOfLife)
	}
	if len(report.Vulnerabilities) != 3 {
		t.Fatalf("got %d vulnerabilities, want 3", len(report.Vulnerabilities))
	}
	ssl := report.Vulnerabilities[0]
	if ssl.ID != "CVE-2023-5363" || ssl.Severity != "high" || ssl.Score != 7.5 || ssl.Fixed != "3.1.4-r0" || !ssl.Fixable() {
		t.Errorf("openssl finding parsed wrong: %+v", ssl)
	}
	if ssl.Target != "(alpine 3.18.0)" || ssl.Class != "os-pkgs" || ssl.Type != "alpine" {
		t.Errorf("openssl target/class/type = %q/%q/%q", ssl.Target, ssl.Class, ssl.Type)
	}
	if got := stripTarget("sbom.spdx.json (alpine 3.20.10)"); got != "(alpine 3.20.10)" {
		t.Errorf("an inventory-matched OS target = %q", got)
	}
	if got := stripTarget("app/node_modules/lodash/package.json"); got != "app/node_modules/lodash/package.json" {
		t.Errorf("a relative package path must stay as it is, got %q", got)
	}
	bb := report.Vulnerabilities[1]
	if bb.Fixable() || bb.Status != "affected" || bb.Score != 3.1 {
		t.Errorf("unfixed finding parsed wrong: %+v", bb)
	}
	lodash := report.Vulnerabilities[2]
	if lodash.Severity != "critical" || lodash.Path != "app/node_modules/lodash/package.json" || lodash.Target != "Node.js" {
		t.Errorf("lodash finding parsed wrong: %+v", lodash)
	}
	if len(report.Secrets) != 1 || report.Secrets[0].Rule != "aws-access-key-id" || report.Secrets[0].Target != "app/.env" || report.Secrets[0].Line != 2 {
		t.Errorf("secret parsed wrong: %+v", report.Secrets)
	}
	// The record is what the console and the assistant read, so it must
	// round-trip as JSON with stable names.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"CVE-2019-10744"`, `"fixed":"4.17.12"`, `"endOfLife":true`, `"rule":"aws-access-key-id"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("record lacks %s:\n%s", want, raw)
		}
	}
}

var awsKeyFixture = Secret{Target: "app/.env", Rule: "aws-access-key-id", Title: "AWS Access Key ID", Severity: "critical", Line: 2, Match: "AWS_ACCESS_KEY_ID=****"}

func TestParseTrivyReportBoundsDescriptions(t *testing.T) {
	long := strings.Repeat("x", 2*maxDescription)
	raw := `{"Results":[{"Target":"t","Class":"lang-pkgs","Type":"pip","Vulnerabilities":[{"VulnerabilityID":"CVE-1","Severity":"HIGH","Description":"` + long + `"}]}]}`
	report, err := parseTrivyReport([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(report.Vulnerabilities[0].Description); got > maxDescription+len("…") {
		t.Errorf("description kept %d bytes, want at most %d", got, maxDescription)
	}
}

func TestParseTrivyReportRefusesGarbage(t *testing.T) {
	if _, err := parseTrivyReport([]byte("not json")); err == nil {
		t.Fatal("garbage parsed as a report")
	}
}

// The step is a shell script run in the scanner image; it must parse, and
// it must stay offline by construction: an inventory and a secret scan
// need no database and no package registry.
func TestScanScript(t *testing.T) {
	script := scanScript(ScanSpec{Secrets: true})
	for _, want := range []string{
		"trivy rootfs --format spdx-json --offline-scan --skip-db-update --skip-java-db-update --output /out/sbom.spdx.json /target",
		"trivy rootfs --scanners secret --offline-scan --skip-db-update --skip-java-db-update --format json --output /out/secrets.json /target",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if plain := scanScript(ScanSpec{}); strings.Contains(plain, "secret") || strings.Contains(plain, "sbom") && strings.Contains(plain, "trivy sbom") {
		t.Errorf("a plain scan should only inventory:\n%s", plain)
	}
	if runtime.GOOS == "windows" {
		return
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell to check against")
	}
	path := filepath.Join(t.TempDir(), "scan.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("the scan script is not valid shell: %v\n%s", err, out)
	}
}
