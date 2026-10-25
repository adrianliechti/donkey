package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// The vulnerability scan is the startup check's sibling: the built image is
// not exported yet, the scanner image is the root of one more LLB step, and
// the image under test is mounted read-only beside it. Nothing the policy
// refuses is ever pushed under the app's name.
//
// The step itself needs no network and no state: it writes an inventory of
// the image (an SPDX SBOM, which the scanner annotates with the package and
// OS details its matching relies on) and reads credential-shaped strings
// off the files. Matching the inventory against the vulnerability database
// is the orchestrator's job (Scanner, trivy.go): the database lives on its
// data volume, so it is fetched once and kept, and a later reading of the
// same image is a lookup against the stored inventory — never an image
// pull, never a build.
const (
	scanTargetDir   = "/target"
	scanOutDir      = "/out"
	scanReportFile  = "report.json"
	scanSecretsFile = "secrets.json"
	scanSBOMFile    = "sbom.spdx.json"
	// The step's scanner gets a scratch cache: it never touches a database.
	scanStepCacheDir = "/tmp/trivy"
)

// ScanSpec is what a build should check its image for before publishing it.
type ScanSpec struct {
	// Image is the scanner (Trivy); pulled by the build engine like any FROM.
	Image string
	// Secrets also looks for credentials baked into the image.
	Secrets bool
	// Matcher fills the findings from the inventory the step wrote (the
	// orchestrator's Scanner). Nil leaves the report with its inventory and
	// secrets only.
	Matcher Matcher
	// Judge decides from the report whether the image may be published; its
	// error ends the build before the export. Nil publishes regardless and
	// keeps the report on the Result.
	Judge func(ctx context.Context, report *ScanReport) error
	// Platform must be explicit, as for VerifySpec.
	Platform ocispecs.Platform
}

// Matcher turns an inventory into findings.
type Matcher interface {
	Match(ctx context.Context, report *ScanReport) error
}

// ScanReport is what the scanner found, reduced to what donkey acts on and
// shows. The scanner's full report is not kept: its descriptions and
// reference lists run to megabytes for a full base image.
type ScanReport struct {
	Scanner string `json:"scanner"`
	// OS is what the image is built on, as the scanner recognized it.
	OS string `json:"os,omitempty"`
	// EndOfLife marks an OS release its vendor no longer patches: findings
	// in it will never get a fixed version.
	EndOfLife bool `json:"endOfLife,omitempty"`
	// DBUpdatedAt is the vulnerability database's own timestamp, so a later
	// scan can say what is new since.
	DBUpdatedAt     string          `json:"dbUpdatedAt,omitempty"`
	DBNextUpdate    string          `json:"dbNextUpdate,omitempty"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
	Secrets         []Secret        `json:"secrets"`
	// SBOM is the inventory the findings were matched from — the caller
	// keeps it for later scans (see ScanSpec.SBOM). Not part of the
	// report's own JSON: it is stored beside it, compressed.
	SBOM []byte `json:"-"`
}

// Vulnerability is one known weakness in one package of the image.
type Vulnerability struct {
	ID string `json:"id"`
	// Severity is lower-case: critical, high, medium, low or unknown.
	Severity string  `json:"severity"`
	Score    float64 `json:"score,omitempty"`
	Package  string  `json:"package"`
	// Installed is the version in the image; Fixed the first version
	// without the weakness, empty when the vendor has published none.
	Installed string `json:"installed"`
	Fixed     string `json:"fixed,omitempty"`
	// Status is the vendor's word: fixed, affected, will_not_fix,
	// end_of_life, fix_deferred, …
	Status string `json:"status,omitempty"`
	// Class is os-pkgs or lang-pkgs; Type names the ecosystem (alpine,
	// debian, node-pkg, python-pkg, gobinary, jar, …).
	Class string `json:"class"`
	Type  string `json:"type"`
	// Target and Path say where in the image: the OS, or the package file.
	Target      string `json:"target,omitempty"`
	Path        string `json:"path,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Published   string `json:"published,omitempty"`
}

// Secret is a credential-shaped string found in a file of the image. Match
// is the line as the scanner reports it, with the secret itself masked.
type Secret struct {
	Target   string `json:"target"`
	Rule     string `json:"rule"`
	Category string `json:"category,omitempty"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Line     int    `json:"line,omitempty"`
	Match    string `json:"match,omitempty"`
}

// Fixable says whether the vendor has published a version without it.
func (v Vulnerability) Fixable() bool { return v.Fixed != "" }

// ScanBlockedError means the image was built and scanned, and the policy
// refused to publish it. The policy lives with the caller; this type lets
// the build loop tell a refused image from a build that failed.
type ScanBlockedError struct {
	Message string
	Report  *ScanReport
}

func (e *ScanBlockedError) Error() string { return e.Message }

// ScanError means the scanner could not produce a verdict. Repairing the
// app cannot fix scanner infrastructure; the deploy can be retried once it
// is available again.
type ScanError struct{ Err error }

func (e *ScanError) Error() string {
	return fmt.Sprintf("the vulnerability scan could not complete: %v", e.Err)
}
func (e *ScanError) Unwrap() error { return e.Err }

// scanState runs the scanner against the image's root filesystem and leaves
// its inventory (and the secrets it found) on a scratch mount, which the
// caller reads back.
func scanState(target llb.State, spec ScanSpec) llb.State {
	st := llb.Image(spec.Image, llb.Platform(spec.Platform)).
		AddEnv("TRIVY_CACHE_DIR", scanStepCacheDir).
		AddEnv("TRIVY_NO_PROGRESS", "true").
		// The scanner would otherwise call home for a version check and
		// usage telemetry — pointless noise, and an error in a build
		// engine that reaches only its mirrors.
		AddEnv("TRIVY_SKIP_VERSION_CHECK", "true").
		AddEnv("TRIVY_DISABLE_TELEMETRY", "true")
	return st.Run(
		llb.Args([]string{"/bin/sh", "-c", scanScript(spec)}),
		llb.WithCustomName("[scan] inventory the image and look for secrets"),
		// A scan must run every time: the policy and the database move
		// under an unchanged image.
		llb.IgnoreCache,
		llb.AddMount(scanTargetDir, target, llb.Readonly),
	).AddMount(scanOutDir, llb.Scratch())
}

// scanScript is written against the scanner image's own shell. Both
// commands are offline by construction: an inventory needs no database, and
// --offline-scan keeps the scanner from asking package registries about
// what it finds.
func scanScript(spec ScanSpec) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "trivy rootfs --format spdx-json --offline-scan --skip-db-update --skip-java-db-update --output %s/%s %s\n", scanOutDir, scanSBOMFile, scanTargetDir)
	if spec.Secrets {
		fmt.Fprintf(&b, "trivy rootfs --scanners secret --offline-scan --skip-db-update --skip-java-db-update --format json --output %s/%s %s\n", scanOutDir, scanSecretsFile, scanTargetDir)
	}
	return b.String()
}

// scan runs the step on the gateway, reads the inventory and secrets back,
// and has the matcher fill in the findings.
func scan(ctx context.Context, gw gateway.Client, target llb.State, spec ScanSpec) (*ScanReport, error) {
	def, err := scanState(target, spec).Marshal(ctx, llb.Platform(spec.Platform))
	if err != nil {
		return nil, fmt.Errorf("preparing the vulnerability scan: %w", err)
	}
	res, err := gw.Solve(ctx, gateway.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, fmt.Errorf("the vulnerability scan did not run: %w", err)
	}
	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}
	sbom, err := ref.ReadFile(ctx, gateway.ReadRequest{Filename: scanSBOMFile})
	if err != nil {
		return nil, fmt.Errorf("the vulnerability scan left no inventory: %w", err)
	}
	report := &ScanReport{Scanner: spec.Image, Vulnerabilities: []Vulnerability{}, Secrets: []Secret{}, SBOM: sbom}
	if spec.Secrets {
		found, err := ref.ReadFile(ctx, gateway.ReadRequest{Filename: scanSecretsFile})
		if err != nil {
			return nil, fmt.Errorf("the secret scan left no report: %w", err)
		}
		secrets, err := parseTrivyReport(found)
		if err != nil {
			return nil, err
		}
		report.Secrets = secrets.Secrets
	}
	if spec.Matcher != nil {
		if err := spec.Matcher.Match(ctx, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// trivyReport is the part of the scanner's JSON donkey reads.
type trivyReport struct {
	Metadata struct {
		OS *struct {
			Family string `json:"Family"`
			Name   string `json:"Name"`
			EOSL   bool   `json:"EOSL"`
		} `json:"OS"`
	} `json:"Metadata"`
	Results []struct {
		Target          string `json:"Target"`
		Class           string `json:"Class"`
		Type            string `json:"Type"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			PkgPath          string `json:"PkgPath"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			Status           string `json:"Status"`
			Severity         string `json:"Severity"`
			Title            string `json:"Title"`
			Description      string `json:"Description"`
			PrimaryURL       string `json:"PrimaryURL"`
			PublishedDate    string `json:"PublishedDate"`
			CVSS             map[string]struct {
				V3Score  float64 `json:"V3Score"`
				V40Score float64 `json:"V40Score"`
			} `json:"CVSS"`
		} `json:"Vulnerabilities"`
		Secrets []struct {
			RuleID    string `json:"RuleID"`
			Category  string `json:"Category"`
			Severity  string `json:"Severity"`
			Title     string `json:"Title"`
			StartLine int    `json:"StartLine"`
			Match     string `json:"Match"`
		} `json:"Secrets"`
	} `json:"Results"`
}

const maxDescription = 400

func parseTrivyReport(raw []byte) (*ScanReport, error) {
	var in trivyReport
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("the scanner's report was not readable: %w", err)
	}
	out := &ScanReport{Vulnerabilities: []Vulnerability{}, Secrets: []Secret{}}
	if in.Metadata.OS != nil {
		out.OS = strings.TrimSpace(in.Metadata.OS.Family + " " + in.Metadata.OS.Name)
		out.EndOfLife = in.Metadata.OS.EOSL
	}
	for _, r := range in.Results {
		target := stripTarget(r.Target)
		for _, v := range r.Vulnerabilities {
			score := 0.0
			if nvd, ok := v.CVSS["nvd"]; ok && nvd.V3Score > 0 {
				score = nvd.V3Score
			} else {
				for _, c := range v.CVSS {
					score = max(score, c.V3Score, c.V40Score)
				}
			}
			out.Vulnerabilities = append(out.Vulnerabilities, Vulnerability{
				ID:          v.VulnerabilityID,
				Severity:    strings.ToLower(v.Severity),
				Score:       score,
				Package:     v.PkgName,
				Installed:   v.InstalledVersion,
				Fixed:       v.FixedVersion,
				Status:      v.Status,
				Class:       r.Class,
				Type:        r.Type,
				Target:      target,
				Path:        stripTarget(v.PkgPath),
				Title:       v.Title,
				Description: truncate(v.Description, maxDescription),
				URL:         v.PrimaryURL,
				Published:   v.PublishedDate,
			})
		}
		for _, s := range r.Secrets {
			out.Secrets = append(out.Secrets, Secret{
				Target:   target,
				Rule:     s.RuleID,
				Category: s.Category,
				Severity: strings.ToLower(s.Severity),
				Title:    s.Title,
				Line:     s.StartLine,
				Match:    truncate(s.Match, 200),
			})
		}
	}
	return out, nil
}

// stripTarget drops the mount point from what the scanner names: the OS
// result is "/target (alpine 3.20.3)" when read off the image and
// "sbom.spdx.json (alpine 3.20.3)" when matched from the inventory; a
// package file is "/target/app/…" or already relative.
func stripTarget(s string) string {
	s = strings.TrimPrefix(s, scanSBOMFile)
	s = strings.TrimPrefix(s, scanTargetDir)
	return strings.TrimSpace(strings.TrimPrefix(s, "/"))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
