package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

// The scan policy lives here, not in the build package: the build engine
// produces the report and asks one question — may this image be published?
// — and the answer depends on the installation's rules, the space's waivers
// and the assistant's reading of the source, none of which a builder knows.

// AppScan is what the scan of a release found and what the policy made of
// it, kept with the release record: the console's Deployment tab shows it,
// and a rollback to the image re-reads it against the policy of that day.
type AppScan struct {
	Scanner     string `json:"scanner"`
	OS          string `json:"os,omitempty"`
	EndOfLife   bool   `json:"endOfLife,omitempty"`
	DBUpdatedAt string `json:"dbUpdatedAt,omitempty"`
	// Policy is what the findings were judged against.
	Policy  ScanPolicy  `json:"policy"`
	Summary ScanSummary `json:"summary"`
	// Findings are what the deploy stream said, in order.
	Findings        []Finding             `json:"findings"`
	Vulnerabilities []build.Vulnerability `json:"vulnerabilities"`
	Secrets         []build.Secret        `json:"secrets"`
	// Verdicts are the assistant's readings of the findings that would have
	// refused the image, whether or not they waived anything.
	Verdicts []ai.Verdict `json:"verdicts,omitempty"`
	// Blocked says the policy refused the image.
	Blocked bool `json:"blocked"`
	// ScannedAt is when the image was read.
	ScannedAt string `json:"scannedAt,omitempty"`
	// SBOM is the scanner's own inventory of the image (SPDX), gzip and
	// base64 so the record stays one JSON document. It is what a later
	// scan of the same image reads instead of the image.
	SBOM string `json:"sbom,omitempty"`
}

// The states a running app's security badge can show.
const (
	// SecurityClean: nothing with a fix at or above the threshold, no
	// credential in the image.
	SecurityClean = "clean"
	// SecurityFixable: fixes are available below the threshold.
	SecurityFixable = "fixable"
	// SecurityWarning: a reported secret or unsupported OS needs attention,
	// but the policy allowed deployment.
	SecurityWarning = "warning"
	// SecurityWaived: the image runs because a space admin or the
	// assistant waived what would have refused it.
	SecurityWaived = "waived"
	// SecurityNeedsAction: the image would be refused today.
	SecurityNeedsAction = "needs-action"
)

// AppSecurity is the scan's outcome as the running app carries it: what a
// fleet row, a space tile or the app's page shows without opening the
// release. It travels on the workload (an annotation, a label) the way the
// stack does, so the control plane stays stateless.
type AppSecurity struct {
	State       string      `json:"state"`
	Summary     ScanSummary `json:"summary"`
	ScannedAt   string      `json:"scannedAt,omitempty"`
	DBUpdatedAt string      `json:"dbUpdatedAt,omitempty"`
}

// securityOf reads the badge off a record.
func securityOf(record *AppScan) *AppSecurity {
	if record == nil {
		return nil
	}
	state := SecurityClean
	switch {
	case record.Blocked:
		state = SecurityNeedsAction
	case record.Summary.Waived+record.Summary.Judged > 0:
		state = SecurityWaived
	case record.EndOfLife || (record.Policy.Secrets != config.ScanSecretsOff && record.Summary.Secrets > 0):
		state = SecurityWarning
	case record.Summary.Fixable > 0:
		state = SecurityFixable
	}
	return &AppSecurity{State: state, Summary: record.Summary, ScannedAt: record.ScannedAt, DBUpdatedAt: record.DBUpdatedAt}
}

// securityJSON is the fact as the runtime keeps it; "" for no scan.
func securityJSON(record *AppScan) string {
	security := securityOf(record)
	if security == nil {
		return ""
	}
	raw, err := json.Marshal(security)
	if err != nil {
		return ""
	}
	return string(raw)
}

// parseSecurity reads the fact back; nil for none or for one an older
// version wrote in a shape this one does not read.
func parseSecurity(raw string) *AppSecurity {
	if raw == "" {
		return nil
	}
	var security AppSecurity
	if json.Unmarshal([]byte(raw), &security) != nil || security.State == "" {
		return nil
	}
	return &security
}

// Shown is the record as the deploy stream carries it: everything but the
// inventory, which only the stored record needs.
func (a *AppScan) Shown() *AppScan {
	if a == nil {
		return nil
	}
	shown := *a
	shown.SBOM = ""
	return &shown
}

// encodeSBOM packs the inventory for the record; an inventory is text that
// compresses several times over.
func encodeSBOM(sbom []byte) string {
	if len(sbom) == 0 {
		return ""
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(sbom); err != nil {
		return ""
	}
	if err := zw.Close(); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// SBOMBytes unpacks the stored inventory; nil when the record has none.
func (a *AppScan) SBOMBytes() ([]byte, error) {
	if a == nil || a.SBOM == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(a.SBOM)
	if err != nil {
		return nil, fmt.Errorf("the stored inventory is not readable: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the stored inventory is not readable: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

// ScanPolicy is the installation's rules, resolved, plus the space's
// waivers for the app in question.
type ScanPolicy struct {
	Block         string                  `json:"block"`
	IgnoreUnfixed bool                    `json:"ignoreUnfixed"`
	Secrets       string                  `json:"secrets"`
	Triage        bool                    `json:"triage"`
	Waivers       []config.SecurityWaiver `json:"waivers,omitempty"`
}

// ScanSummary is the report in numbers, for the places that show one line.
type ScanSummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown,omitempty"`
	Fixable  int `json:"fixable"`
	Unfixed  int `json:"unfixed"`
	Secrets  int `json:"secrets"`
	// Blocking is what refused, or would have refused, the image before
	// any waiver or verdict; Waived and Judged are what let it through.
	Blocking int `json:"blocking"`
	Waived   int `json:"waived"`
	Judged   int `json:"judged"`
}

// Finding codes the console keys off. The level says what the deploy did;
// the code says why.
const (
	FindingVulnBlocking = "vuln-blocking" // block: a fixable finding at or above the threshold
	FindingVulnJudged   = "vuln-judged"   // warn: the assistant read the app as not affected
	FindingVulnWaived   = "vuln-waived"   // warn: a space admin accepted it
	FindingVulnFixable  = "vuln-fixable"  // warn: fixes available below the threshold
	FindingVulnUnfixed  = "vuln-unfixed"  // info: no fix published yet
	FindingVulnClean    = "vuln-clean"    // info: nothing with a fix
	FindingOSEndOfLife  = "os-end-of-life"
	FindingSecretImage  = "secret-in-image"
)

var severityRank = map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}

// scanPolicy resolves the policy for one app: the installation's settings
// plus the space's waivers still in force.
func (s *Server) scanPolicy(ctx context.Context, space, app string) (ScanPolicy, error) {
	settings := s.cfg.Build.Scan
	policy := ScanPolicy{
		Block:         settings.BlockSeverity(),
		IgnoreUnfixed: settings.IgnoreUnfixedFindings(),
		Secrets:       settings.SecretsPolicy(),
		Triage:        settings.AssistantTriage(),
	}
	spaceSettings, err := s.spaceSettings(ctx, space)
	if err != nil {
		return ScanPolicy{}, &build.ScanError{Err: fmt.Errorf("reading the space's scan policy: %w", err)}
	}
	policy.Waivers = spaceSettings.WaiversFor(app, time.Now())
	return policy, nil
}

func (p ScanPolicy) threshold() int {
	switch p.Block {
	case config.ScanBlockCritical:
		return severityRank["critical"]
	case config.ScanBlockHigh:
		return severityRank["high"]
	}
	return math.MaxInt
}

// waiver finds an admin's waiver for a finding. A secret is waived by its
// rule, or by rule@target to keep it to one file.
func (p ScanPolicy) waiver(ids ...string) (config.SecurityWaiver, bool) {
	for _, w := range p.Waivers {
		for _, id := range ids {
			if strings.EqualFold(w.ID, id) {
				return w, true
			}
		}
	}
	return config.SecurityWaiver{}, false
}

// newScanner is the orchestrator's matcher: the scanner binary in the
// server image, its database under scanner/ on the data volume beside the
// source history, the installation's mirrors, and the registry's
// credentials and trust for them.
func newScanner(cfg *Config) *build.Scanner {
	dir := filepath.Join(cfg.DataDir, "scanner")
	if cfg.DataDir == "" {
		// No volume (a dev server, a test): the database still has to live
		// somewhere between scans.
		dir = filepath.Join(os.TempDir(), "donkey-scanner")
	}
	return &build.Scanner{
		Dir:              dir,
		DBRepository:     cfg.Build.Scan.DBRepository,
		JavaDBRepository: cfg.Build.Scan.JavaDBRepository,
		Registry:         cfg.Registry,
	}
}

// scanSpec is the scan as the build engine runs it; nil when scanning is
// off. The judge is attached per attempt, where the recipe is known.
func (s *Server) scanSpec() *build.ScanSpec {
	settings := s.cfg.Build.Scan
	if !settings.IsEnabled() {
		return nil
	}
	spec := &build.ScanSpec{
		Image:   settings.ScannerImage(),
		Secrets: settings.SecretsPolicy() != config.ScanSecretsOff,
	}
	if s.scanner != nil {
		spec.Matcher = s.scanner
	}
	return spec
}

// scanJudge is the policy as the build engine calls it: the report is
// evaluated, the findings that would refuse the image go to the assistant,
// the outcome is narrated and recorded, and a refusal ends the build before
// any export. root is the build workspace the image came from and appDir
// the app's folder inside it; recipe the Dockerfile of this attempt.
func (s *Server) scanJudge(out *lineStreamer, app *config.AppConfig, root, appDir, recipe string, known []string) func(context.Context, *build.ScanReport) error {
	return func(ctx context.Context, report *build.ScanReport) error {
		policy, err := s.scanPolicy(ctx, app.Space, app.Name)
		if err != nil {
			return err
		}
		eval := evaluateScan(report, policy)
		verdicts := map[string]ai.Verdict{}
		if len(eval.Blocking) > 0 && policy.Triage {
			out.Line("· %s would refuse this image — asking the assistant whether %s is actually affected…", countOf(len(eval.Blocking), "finding"), app.Name)
			triage, err := s.assistant.TriageVulnerabilities(ctx, &ai.TriageInput{
				Root: root, AppDir: appDir, Recipe: recipe, Exposure: exposureOfConfig(app),
				Findings: triageFindings(eval.Blocking), KnownSecrets: known,
				Progress: func(line string) { out.Line("· %s", ai.RedactEvidence(line, known...)) },
			})
			if err != nil {
				out.Line("· The assistant could not judge the findings, so the policy stands: %s", ai.RedactEvidence(err.Error(), known...))
			} else {
				for _, v := range triage.Verdicts {
					verdicts[v.ID] = v
				}
			}
		}
		record := scanRecord(report, policy, eval, verdicts)
		record.ScannedAt = time.Now().UTC().Format(time.RFC3339)
		for _, f := range record.Findings {
			out.Line("· %s", f.Text())
		}
		out.Scan(record)
		if record.Blocked {
			return &build.ScanBlockedError{Message: refusalMessage(record), Report: report}
		}
		return nil
	}
}

// checkStoredScan applies today's policy to the scan recorded with an image
// a deploy reuses (--no-build --tag, a rollback): deploying is deploying.
// An image with no record — built before scanning, or with it off — is
// deployed as it is, and the stream says so. The record it returns is what
// the deploy stamps the workload with.
func (s *Server) checkStoredScan(ctx context.Context, out *lineStreamer, space string, app *config.AppConfig, imageRef string) (*AppScan, error) {
	if !s.cfg.Build.Scan.IsEnabled() {
		return nil, nil
	}
	raw, err := s.telemetry.BuildByImage(ctx, space, app.Name, imageRef)
	if err != nil && !errors.Is(err, telemetry.ErrNoBuild) {
		return nil, fmt.Errorf("reading the image's recorded scan; retry when build history is available: %w", err)
	}
	var release AppRelease
	if err == nil {
		if err := json.Unmarshal(raw, &release); err != nil {
			return nil, fmt.Errorf("the image's recorded scan is not readable: %w", err)
		}
	}
	if release.Scan == nil {
		out.Line("· No vulnerability scan is on record for this image; it is deployed as it is")
		return nil, nil
	}
	stored := release.Scan
	report := &build.ScanReport{
		Scanner: stored.Scanner, OS: stored.OS, EndOfLife: stored.EndOfLife, DBUpdatedAt: stored.DBUpdatedAt,
		Vulnerabilities: stored.Vulnerabilities, Secrets: stored.Secrets,
	}
	policy, err := s.scanPolicy(ctx, space, app.Name)
	if err != nil {
		return nil, err
	}
	verdicts := map[string]ai.Verdict{}
	for _, v := range stored.Verdicts {
		verdicts[v.ID] = v
	}
	record := scanRecord(report, policy, evaluateScan(report, policy), verdicts)
	// Re-evaluation only changes the policy decision, not the inventory.
	record.SBOM = stored.SBOM
	record.ScannedAt = stored.ScannedAt
	when := ""
	if stored.DBUpdatedAt != "" {
		when = " (database from " + shortDate(stored.DBUpdatedAt) + ")"
	}
	out.Line("· Vulnerability scan on record%s: %s", when, record.Summary.Text())
	for _, f := range record.Findings {
		if f.Level != LevelInfo {
			out.Line("· %s", f.Text())
		}
	}
	out.Scan(record)
	if record.Blocked {
		return record, fmt.Errorf("%s", refusalMessage(record))
	}
	return record, nil
}

// scanEvaluation separates a report into what the policy makes of it,
// before the assistant has said anything.
type scanEvaluation struct {
	// Blocking would refuse the image; a waiving verdict is the only way out.
	Blocking []build.Vulnerability
	Waived   []waivedVulnerability
	// Fixable are below the threshold but have a fix; Unfixed have none.
	Fixable []build.Vulnerability
	Unfixed []build.Vulnerability
	// Secrets by what the policy says about them.
	BlockingSecrets []build.Secret
	WarnSecrets     []build.Secret
	WaivedSecrets   []waivedSecret
}

type waivedVulnerability struct {
	build.Vulnerability
	Waiver config.SecurityWaiver
}

type waivedSecret struct {
	build.Secret
	Waiver config.SecurityWaiver
}

func evaluateScan(report *build.ScanReport, policy ScanPolicy) scanEvaluation {
	var eval scanEvaluation
	threshold := policy.threshold()
	vulns := append([]build.Vulnerability{}, report.Vulnerabilities...)
	// Worst first, then by package, so every rendering reads the same way.
	sort.SliceStable(vulns, func(i, j int) bool {
		if a, b := severityRank[vulns[i].Severity], severityRank[vulns[j].Severity]; a != b {
			return a > b
		}
		if vulns[i].Package != vulns[j].Package {
			return vulns[i].Package < vulns[j].Package
		}
		return vulns[i].ID < vulns[j].ID
	})
	for _, v := range vulns {
		switch {
		case !v.Fixable() && policy.IgnoreUnfixed:
			eval.Unfixed = append(eval.Unfixed, v)
		case severityRank[v.Severity] < threshold:
			if v.Fixable() {
				eval.Fixable = append(eval.Fixable, v)
			} else {
				eval.Unfixed = append(eval.Unfixed, v)
			}
		default:
			if w, ok := policy.waiver(v.ID); ok {
				eval.Waived = append(eval.Waived, waivedVulnerability{v, w})
			} else {
				eval.Blocking = append(eval.Blocking, v)
			}
		}
	}
	for _, sec := range report.Secrets {
		if policy.Secrets == config.ScanSecretsOff {
			continue
		}
		if w, ok := policy.waiver(sec.Rule, sec.Rule+"@"+sec.Target); ok {
			eval.WaivedSecrets = append(eval.WaivedSecrets, waivedSecret{sec, w})
			continue
		}
		if policy.Secrets == config.ScanSecretsBlock {
			eval.BlockingSecrets = append(eval.BlockingSecrets, sec)
		} else {
			eval.WarnSecrets = append(eval.WarnSecrets, sec)
		}
	}
	return eval
}

// scanRecord turns the evaluation and the assistant's verdicts into the
// findings the deploy says and the record it keeps.
func scanRecord(report *build.ScanReport, policy ScanPolicy, eval scanEvaluation, verdicts map[string]ai.Verdict) *AppScan {
	record := &AppScan{
		Scanner: report.Scanner, OS: report.OS, EndOfLife: report.EndOfLife, DBUpdatedAt: report.DBUpdatedAt,
		Policy:          policy,
		Findings:        []Finding{},
		Vulnerabilities: append([]build.Vulnerability{}, report.Vulnerabilities...),
		Secrets:         append([]build.Secret{}, report.Secrets...),
		SBOM:            encodeSBOM(report.SBOM),
	}
	for _, v := range report.Vulnerabilities {
		switch v.Severity {
		case "critical":
			record.Summary.Critical++
		case "high":
			record.Summary.High++
		case "medium":
			record.Summary.Medium++
		case "low":
			record.Summary.Low++
		default:
			record.Summary.Unknown++
		}
		if v.Fixable() {
			record.Summary.Fixable++
		} else {
			record.Summary.Unfixed++
		}
	}
	record.Summary.Secrets = len(report.Secrets)
	record.Summary.Blocking = len(eval.Blocking) + len(eval.Waived) + len(eval.BlockingSecrets) + len(eval.WaivedSecrets)
	record.Summary.Waived = len(eval.Waived) + len(eval.WaivedSecrets)
	// Keep verdicts even while an admin waiver or a relaxed threshold makes
	// them unnecessary. A later rollback can need them again.
	seenVerdicts := map[string]bool{}
	for _, v := range report.Vulnerabilities {
		if verdict, ok := verdicts[v.ID]; ok && !seenVerdicts[v.ID] {
			record.Verdicts = append(record.Verdicts, verdict)
			seenVerdicts[v.ID] = true
		}
	}

	for _, v := range eval.Blocking {
		verdict, judged := verdicts[v.ID]
		if judged && policy.Triage && verdict.Waives() {
			record.Summary.Judged++
			record.Findings = append(record.Findings, Finding{
				Level:  LevelWarn,
				Code:   FindingVulnJudged,
				Title:  fmt.Sprintf("%s — the assistant read %s and judged this app not affected", vulnerabilityHeadline(v), app(v)),
				Detail: verdictDetail(verdict),
				Fix:    verdictFix(v, verdict),
			})
			continue
		}
		record.Blocked = true
		f := Finding{
			Level:  LevelBlock,
			Code:   FindingVulnBlocking,
			Title:  vulnerabilityHeadline(v),
			Detail: vulnerabilityDetail(v),
			Fix:    vulnerabilityFix(v),
			File:   v.Path,
		}
		if judged {
			f.Detail = strings.TrimSpace(f.Detail + "\n" + verdictDetail(verdict))
			f.Fix = verdictFix(v, verdict)
		}
		record.Findings = append(record.Findings, f)
	}
	for _, w := range eval.Waived {
		record.Findings = append(record.Findings, Finding{
			Level:  LevelWarn,
			Code:   FindingVulnWaived,
			Title:  fmt.Sprintf("%s — waived by %s%s", vulnerabilityHeadline(w.Vulnerability), byName(w.Waiver), untilText(w.Waiver)),
			Detail: w.Waiver.Reason,
		})
	}
	for _, sec := range eval.BlockingSecrets {
		record.Blocked = true
		record.Findings = append(record.Findings, secretFinding(sec, LevelBlock))
	}
	for _, sec := range eval.WarnSecrets {
		record.Findings = append(record.Findings, secretFinding(sec, LevelWarn))
	}
	for _, w := range eval.WaivedSecrets {
		f := secretFinding(w.Secret, LevelWarn)
		f.Title += fmt.Sprintf(" — waived by %s%s", byName(w.Waiver), untilText(w.Waiver))
		f.Detail = w.Waiver.Reason
		f.Fix = ""
		record.Findings = append(record.Findings, f)
	}
	if len(eval.Fixable) > 0 {
		record.Findings = append(record.Findings, Finding{
			Level:  LevelWarn,
			Code:   FindingVulnFixable,
			Title:  fmt.Sprintf("%s below the %s threshold %s a fix available", countOf(len(eval.Fixable), "vulnerability"), policy.Block, have(len(eval.Fixable))),
			Detail: listVulnerabilities(eval.Fixable, 5),
			Fix:    "Update the packages named, or rebuild on a current base image tag — the full list is in the release record.",
		})
	}
	if len(eval.Unfixed) > 0 {
		record.Findings = append(record.Findings, Finding{
			Level:  LevelInfo,
			Code:   FindingVulnUnfixed,
			Title:  fmt.Sprintf("%s %s no fix published yet (%s)", countOf(len(eval.Unfixed), "vulnerability"), have(len(eval.Unfixed)), severityCounts(eval.Unfixed)),
			Detail: "Reported for the record; they never refuse a deploy, because nothing in the app can change them until the vendor publishes a fixed version.",
		})
	}
	if report.EndOfLife && report.OS != "" {
		record.Findings = append(record.Findings, Finding{
			Level:  LevelWarn,
			Code:   FindingOSEndOfLife,
			Title:  fmt.Sprintf("The image's operating system (%s) no longer receives security updates", report.OS),
			Detail: "Its vendor publishes no more fixes, so findings in it stay unfixed for good.",
			Fix:    "Move to a supported base image tag.",
		})
	}
	// One reassuring line when there was nothing to say at all; with
	// findings of any kind, those lines already say what the policy did.
	if len(report.Vulnerabilities) == 0 && len(eval.BlockingSecrets)+len(eval.WarnSecrets)+len(eval.WaivedSecrets) == 0 {
		title := "No known vulnerabilities"
		if report.DBUpdatedAt != "" {
			title += " (database from " + shortDate(report.DBUpdatedAt) + ")"
		}
		record.Findings = append(record.Findings, Finding{Level: LevelInfo, Code: FindingVulnClean, Title: title})
	}
	return record
}

// refusalMessage is the one line a refused deploy ends with.
func refusalMessage(record *AppScan) string {
	var names []string
	for _, f := range record.Findings {
		if f.Level == LevelBlock {
			names = append(names, f.Title)
		}
	}
	msg := "the image was refused by the vulnerability policy"
	switch len(names) {
	case 0:
	case 1:
		msg += ": " + names[0]
	default:
		msg += fmt.Sprintf(": %s, and %d more", names[0], len(names)-1)
	}
	return msg + ". Fix what it names, or ask a space admin to record a waiver in the space settings."
}

// Text is the summary as one line.
func (s ScanSummary) Text() string {
	parts := []string{}
	for _, c := range []struct {
		n    int
		name string
	}{{s.Critical, "critical"}, {s.High, "high"}, {s.Medium, "medium"}, {s.Low, "low"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.name))
		}
	}
	if len(parts) == 0 {
		return "no known vulnerabilities"
	}
	out := strings.Join(parts, ", ")
	out += fmt.Sprintf("; %d with a fix", s.Fixable)
	if s.Secrets > 0 {
		out += fmt.Sprintf("; %s", countOf(s.Secrets, "secret"))
	}
	return out
}

func triageFindings(vulns []build.Vulnerability) []ai.TriageFinding {
	out := make([]ai.TriageFinding, 0, len(vulns))
	for _, v := range vulns {
		out = append(out, ai.TriageFinding{
			ID: v.ID, Severity: v.Severity, Package: v.Package, Installed: v.Installed, Fixed: v.Fixed,
			Class: v.Class, Type: v.Type, Path: v.Path, Title: v.Title, Description: v.Description,
		})
	}
	return out
}

func vulnerabilityHeadline(v build.Vulnerability) string {
	s := fmt.Sprintf("%s: %s %s has a %s vulnerability", v.ID, v.Package, v.Installed, v.Severity)
	if v.Fixed != "" {
		s += ", fixed in " + v.Fixed
	}
	return s
}

func vulnerabilityDetail(v build.Vulnerability) string {
	parts := []string{}
	if v.Title != "" {
		parts = append(parts, v.Title)
	}
	where := ""
	switch {
	case v.Path != "":
		where = "in the image at " + v.Path
	case v.Class == "os-pkgs":
		where = "an operating-system package of the base image"
	case v.Target != "":
		where = "in " + v.Target
	}
	if where != "" {
		parts = append(parts, strings.ToUpper(where[:1])+where[1:]+".")
	}
	if v.URL != "" {
		parts = append(parts, v.URL)
	}
	return strings.Join(parts, " ")
}

func vulnerabilityFix(v build.Vulnerability) string {
	if v.Class == "os-pkgs" {
		if v.Fixed != "" {
			return fmt.Sprintf("Rebuild on a current tag of the base image, or upgrade %s to %s in the Dockerfile.", v.Package, v.Fixed)
		}
		return "Move to a base image whose vendor has fixed it."
	}
	if v.Fixed != "" {
		return fmt.Sprintf("Update %s to %s or newer.", v.Package, v.Fixed)
	}
	return ""
}

func verdictDetail(v ai.Verdict) string {
	s := fmt.Sprintf("The assistant's reading (%s, %s confidence): %s", v.Verdict, v.Confidence, strings.TrimSpace(v.Reason))
	if len(v.Evidence) > 0 {
		s += " Evidence: " + strings.Join(v.Evidence, ", ") + "."
	}
	return s
}

func verdictFix(v build.Vulnerability, verdict ai.Verdict) string {
	if strings.TrimSpace(verdict.Fix) != "" {
		return strings.TrimSpace(verdict.Fix)
	}
	return vulnerabilityFix(v)
}

func secretFinding(sec build.Secret, level string) Finding {
	where := sec.Target
	if sec.Line > 0 {
		where = fmt.Sprintf("%s:%d", sec.Target, sec.Line)
	}
	return Finding{
		Level:  level,
		Code:   FindingSecretImage,
		Title:  fmt.Sprintf("A credential is baked into the image: %s in %s", sec.Title, where),
		Detail: sec.Match,
		Fix:    "Keep it out of the image (exclude the file from the build, or read it from an environment variable at runtime) and rotate it if it is real. A space admin can waive the rule " + sec.Rule + " for a value that only looks like one.",
		File:   sec.Target,
	}
}

func listVulnerabilities(vulns []build.Vulnerability, limit int) string {
	lines := []string{}
	for i, v := range vulns {
		if i == limit {
			lines = append(lines, fmt.Sprintf("… and %d more", len(vulns)-limit))
			break
		}
		lines = append(lines, fmt.Sprintf("%s in %s %s → %s (%s)", v.ID, v.Package, v.Installed, v.Fixed, v.Severity))
	}
	return strings.Join(lines, "; ")
}

func severityCounts(vulns []build.Vulnerability) string {
	counts := map[string]int{}
	for _, v := range vulns {
		counts[v.Severity]++
	}
	parts := []string{}
	for _, sev := range []string{"critical", "high", "medium", "low", "unknown"} {
		if counts[sev] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[sev], sev))
		}
	}
	return strings.Join(parts, ", ")
}

func byName(w config.SecurityWaiver) string {
	if w.By == "" {
		return "a space admin"
	}
	return w.By
}

func untilText(w config.SecurityWaiver) string {
	if w.Expires == "" {
		return ""
	}
	return " until " + w.Expires
}

func shortDate(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	return ts
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func have(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// app names the app in a finding title where the vulnerability's package
// already says what; kept short so the line stays one line.
func app(v build.Vulnerability) string {
	if v.Path != "" {
		return "the code using " + v.Package
	}
	return "the app"
}

// scanChecks are doctor's probes for the scanner: the inventory image
// resolves through the build engine (and its mirrors), the scanner binary
// the orchestrator matches with answers, the database is on the volume and
// fresh, and its repository answers — which, in a restricted network, is
// the mirror the operator configured.
func (s *Server) scanChecks(ctx context.Context) []DoctorCheck {
	settings := s.cfg.Build.Scan
	scanner := DoctorCheck{Name: "Vulnerability scanner (Trivy)"}
	if !settings.IsEnabled() {
		scanner.OK = true
		scanner.Detail = "off — images are published without a scan (build.scan.enabled: false)"
		return []DoctorCheck{scanner}
	}
	policy := fmt.Sprintf("refuses %s findings with a fix", settings.BlockSeverity())
	if settings.BlockSeverity() == config.ScanBlockNone {
		policy = "findings are reported, never refused"
	}
	if s.scanner == nil {
		scanner.Detail = "no scanner configured"
	} else if version, err := s.scanner.Version(ctx); err != nil {
		scanner.Detail = err.Error()
	} else if err := build.CheckImageResolves(ctx, s.cfg.BuildkitAddr, s.cfg.Registry, s.cfg.RegistryMirrors, settings.ScannerImage(), s.cfg.DefaultPlatform); err != nil {
		scanner.Detail = fmt.Sprintf("%s does not resolve in the build engine: %v", settings.ScannerImage(), err)
	} else {
		scanner.OK = true
		scanner.Detail = fmt.Sprintf("%s, inventories with %s; %s", version, settings.ScannerImage(), policy)
	}
	checks := []DoctorCheck{scanner}

	db := DoctorCheck{Name: "Vulnerability database"}
	repo := settings.DBRepository
	if repo == "" {
		repo = build.DefaultScannerDBRepository
	}
	status := build.DBStatus{}
	if s.scanner != nil {
		status = s.scanner.DBStatus()
	}
	switch {
	case !status.Present:
		if err := build.CheckOCIArtifact(ctx, s.cfg.Registry, repo); err != nil {
			db.Detail = fmt.Sprintf("not fetched yet, and %s does not answer: %v", repo, err)
			if settings.DBRepository == "" {
				db.Detail += " — a network without internet needs build.scan.dbRepository pointed at a mirror"
			}
		} else {
			db.OK = true
			db.Detail = fmt.Sprintf("fetched from %s on the first scan", repo)
		}
	case time.Since(status.UpdatedAt) > 7*24*time.Hour:
		db.Detail = fmt.Sprintf("stale: last updated %s from %s — scans are blind to anything newer", status.UpdatedAt.UTC().Format("2006-01-02"), repo)
		if err := build.CheckOCIArtifact(ctx, s.cfg.Registry, repo); err != nil {
			db.Detail += fmt.Sprintf("; the repository does not answer: %v", err)
		}
	default:
		db.OK = true
		db.Detail = fmt.Sprintf("updated %s, from %s", status.UpdatedAt.UTC().Format("2006-01-02 15:04"), repo)
	}
	checks = append(checks, db)
	if settings.JavaDBRepository != "" {
		java := DoctorCheck{Name: "Java vulnerability database"}
		if err := build.CheckOCIArtifact(ctx, s.cfg.Registry, settings.JavaDBRepository); err != nil {
			java.Detail = fmt.Sprintf("%s: %v", settings.JavaDBRepository, err)
		} else {
			java.OK = true
			java.Detail = settings.JavaDBRepository + " answers"
		}
		checks = append(checks, java)
	}
	return checks
}
