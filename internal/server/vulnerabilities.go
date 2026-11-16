package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// The vulnerability report turns the fleet's scans around. An app's page
// asks "what is wrong with this app"; whoever answers for the platform's
// security asks "where is this CVE, and who do I talk to". So the findings
// of every running image are grouped by the finding: one entry per
// vulnerability (or credential baked into an image), the apps it is in, what
// today's policy makes of it in each, and who answers for their spaces.

// What the policy makes, today, of one finding in one app.
const (
	// FindingBlocking would refuse the image if it were deployed now.
	FindingBlocking = "blocking"
	// FindingWarning is a credential the policy reports without refusing.
	FindingWarning = "warning"
	// FindingJudged: the assistant read the app's code as not affected.
	FindingJudged = "judged"
	// FindingWaived: a space admin accepted it, for now or for good.
	FindingWaived = "waived"
	// FindingFixable has a fix, and sits below what the policy refuses.
	FindingFixable = "fixable"
	// FindingUnfixed has no fix published.
	FindingUnfixed = "unfixed"
)

// A finding in several packages of one app takes the state that asks most
// of somebody.
var findingStateRank = map[string]int{
	FindingBlocking: 6, FindingWarning: 5, FindingJudged: 4, FindingWaived: 3, FindingFixable: 2, FindingUnfixed: 1,
}

const (
	FindingKindVulnerability = "vulnerability"
	FindingKindSecret        = "secret"
)

// VulnerabilityReport is every finding in the images that run right now.
type VulnerabilityReport struct {
	GeneratedAt string `json:"generatedAt"`
	// Scanning says whether the installation scans images at all, and Block
	// the severity from which its policy refuses one ("none" for never).
	Scanning bool   `json:"scanning"`
	Block    string `json:"block"`
	// Apps is every running app, scanned or not: what the findings cover.
	Apps     []ReportApp    `json:"apps"`
	Findings []FindingGroup `json:"findings"`
	// Spaces names who answers for each space with an app; PlatformAdmins
	// who runs the installation.
	Spaces         map[string]SpaceContacts `json:"spaces"`
	PlatformAdmins config.AccessConfig      `json:"platformAdmins"`
}

// FindingGroup is one finding and every app it is in.
type FindingGroup struct {
	// Kind is vulnerability or secret; ID the CVE or advisory, or the rule
	// that recognized the credential.
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Severity is the worst any app's scan gave it: the same CVE can weigh
	// differently in two distributions.
	Severity  string  `json:"severity"`
	Score     float64 `json:"score,omitempty"`
	Title     string  `json:"title,omitempty"`
	URL       string  `json:"url,omitempty"`
	Published string  `json:"published,omitempty"`
	// Fixable says a fixed version exists for at least one app.
	Fixable bool          `json:"fixable"`
	Apps    []AffectedApp `json:"apps"`
}

// AffectedApp is one app a finding is in.
type AffectedApp struct {
	Space    string `json:"space"`
	App      string `json:"app"`
	Severity string `json:"severity"`
	// State is what today's policy makes of it here; Note the waiver or
	// the assistant's reading behind a state that has one.
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
	// Packages carry a vulnerability, Locations a credential.
	Packages  []AffectedPackage `json:"packages,omitempty"`
	Locations []string          `json:"locations,omitempty"`
}

// AffectedPackage is one package of an image a vulnerability is in.
type AffectedPackage struct {
	Name      string `json:"name"`
	Installed string `json:"installed"`
	// Fixed is the first version without it; empty while none is published.
	Fixed string `json:"fixed,omitempty"`
	// Type names the ecosystem (debian, alpine, node-pkg, python-pkg, …):
	// whether the fix is a new base image or a dependency update.
	Type string `json:"type,omitempty"`
	Path string `json:"path,omitempty"`
}

// findingBuild is a group while its apps are still being collected.
type findingBuild struct {
	FindingGroup
	apps map[runtime.AppRef]*AffectedApp
}

func (g *findingBuild) app(ref runtime.AppRef) *AffectedApp {
	app := g.apps[ref]
	if app == nil {
		app = &AffectedApp{Space: ref.Space, App: ref.App}
		g.apps[ref] = app
	}
	return app
}

func severityOf(severity string) string {
	if _, known := severityRank[severity]; known {
		return severity
	}
	return "unknown"
}

func worseSeverity(a, b string) string {
	if a == "" || severityRank[b] > severityRank[a] {
		return b
	}
	return a
}

// worse records a finding's state in an app, keeping the one that asks most.
func (a *AffectedApp) worse(severity, state, note string) {
	a.Severity = worseSeverity(a.Severity, severity)
	if findingStateRank[state] > findingStateRank[a.State] {
		a.State, a.Note = state, note
	} else if a.Note == "" && state == a.State {
		a.Note = note
	}
}

func waiverNote(w config.SecurityWaiver) string {
	return fmt.Sprintf("Waived by %s%s: %s", byName(w), untilText(w), strings.TrimSpace(w.Reason))
}

// vulnerabilityReport reads the inventory's scans as findings. The policy
// applied is today's — the installation's rules and each space's waivers
// still in force — so a waiver that ran out since the scan shows as what it
// is now.
func (s *Server) vulnerabilityReport(ctx context.Context, inventory *fleetInventory, now time.Time) *VulnerabilityReport {
	report := &VulnerabilityReport{
		GeneratedAt:    inventory.At.UTC().Format(time.RFC3339),
		Scanning:       s.cfg.Build.Scan.IsEnabled(),
		Block:          s.cfg.Build.Scan.BlockSeverity(),
		Apps:           s.reportApps(inventory),
		Findings:       []FindingGroup{},
		Spaces:         reportContacts(inventory),
		PlatformAdmins: platformAdmins(ctx),
	}
	groups := map[string]*findingBuild{}
	group := func(kind, id string) *findingBuild {
		key := kind + "\x00" + id
		g := groups[key]
		if g == nil {
			g = &findingBuild{FindingGroup: FindingGroup{Kind: kind, ID: id}, apps: map[runtime.AppRef]*AffectedApp{}}
			groups[key] = g
		}
		return g
	}

	for _, app := range inventory.Apps {
		if app.Facts == nil || app.Facts.scan == nil {
			continue
		}
		scan := app.Facts.scan
		policy := s.installationPolicy()
		policy.Waivers = inventory.Spaces[app.Space].WaiversFor(app.App, now)
		eval := evaluateScan(&build.ScanReport{Vulnerabilities: scan.Vulnerabilities, Secrets: scan.Secrets}, policy)
		verdicts := scanVerdicts(scan)

		vulnerability := func(v build.Vulnerability, state, note string) {
			severity := severityOf(v.Severity)
			g := group(FindingKindVulnerability, v.ID)
			g.Severity = worseSeverity(g.Severity, severity)
			g.Score = max(g.Score, v.Score)
			g.Fixable = g.Fixable || v.Fixable()
			if g.Title == "" {
				g.Title = v.Title
			}
			if g.URL == "" {
				g.URL = v.URL
			}
			if v.Published != "" && (g.Published == "" || v.Published < g.Published) {
				g.Published = v.Published
			}
			affected := g.app(app.AppRef)
			affected.worse(severity, state, note)
			pkg := AffectedPackage{Name: v.Package, Installed: v.Installed, Fixed: v.Fixed, Type: v.Type, Path: v.Path}
			for _, have := range affected.Packages {
				if have == pkg {
					return
				}
			}
			affected.Packages = append(affected.Packages, pkg)
		}
		for _, v := range eval.Blocking {
			verdict, judged := verdicts[v.ID]
			switch {
			case judged && policy.Triage && verdict.Waives():
				vulnerability(v, FindingJudged, "The assistant read the app as not affected: "+strings.TrimSpace(verdict.Reason))
			case judged:
				vulnerability(v, FindingBlocking, verdictDetail(verdict))
			default:
				vulnerability(v, FindingBlocking, "")
			}
		}
		for _, w := range eval.Waived {
			vulnerability(w.Vulnerability, FindingWaived, waiverNote(w.Waiver))
		}
		for _, v := range eval.Fixable {
			vulnerability(v, FindingFixable, "")
		}
		for _, v := range eval.Unfixed {
			vulnerability(v, FindingUnfixed, "")
		}

		secret := func(sec build.Secret, state, note string) {
			severity := severityOf(sec.Severity)
			g := group(FindingKindSecret, sec.Rule)
			g.Severity = worseSeverity(g.Severity, severity)
			if g.Title == "" {
				g.Title = sec.Title
			}
			affected := g.app(app.AppRef)
			affected.worse(severity, state, note)
			where := sec.Target
			if sec.Line > 0 {
				where = fmt.Sprintf("%s:%d", sec.Target, sec.Line)
			}
			for _, have := range affected.Locations {
				if have == where {
					return
				}
			}
			affected.Locations = append(affected.Locations, where)
		}
		for _, sec := range eval.BlockingSecrets {
			secret(sec, FindingBlocking, "")
		}
		for _, sec := range eval.WarnSecrets {
			secret(sec, FindingWarning, "")
		}
		for _, w := range eval.WaivedSecrets {
			secret(w.Secret, FindingWaived, waiverNote(w.Waiver))
		}
	}

	for _, g := range groups {
		for _, app := range g.apps {
			sort.Slice(app.Packages, func(i, j int) bool {
				if app.Packages[i].Name != app.Packages[j].Name {
					return app.Packages[i].Name < app.Packages[j].Name
				}
				return app.Packages[i].Path < app.Packages[j].Path
			})
			sort.Strings(app.Locations)
			g.Apps = append(g.Apps, *app)
		}
		sort.Slice(g.Apps, func(i, j int) bool {
			if g.Apps[i].Space != g.Apps[j].Space {
				return g.Apps[i].Space < g.Apps[j].Space
			}
			return g.Apps[i].App < g.Apps[j].App
		})
		report.Findings = append(report.Findings, g.FindingGroup)
	}
	// Worst first, then the one in the most apps: the order to work through.
	sort.Slice(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] > severityRank[b.Severity]
		}
		if len(a.Apps) != len(b.Apps) {
			return len(a.Apps) > len(b.Apps)
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})
	return report
}

// findingFilter narrows a report the way the console's list does, so an
// export holds what was on screen. The console applies the same rules in
// vulnerabilities.ts — change both together.
type findingFilter struct {
	// severities empty means every severity.
	severities map[string]bool
	// query is matched lower-case against a finding's id, title and package
	// names, and against the spaces and apps it is in.
	query   string
	fixable bool
}

func parseFindingFilter(values url.Values) findingFilter {
	filter := findingFilter{
		query:   strings.ToLower(strings.TrimSpace(values.Get("q"))),
		fixable: values.Get("fixable") == "true",
	}
	for _, raw := range values["severity"] {
		for severity := range strings.SplitSeq(raw, ",") {
			if severity = strings.ToLower(strings.TrimSpace(severity)); severity != "" {
				if filter.severities == nil {
					filter.severities = map[string]bool{}
				}
				filter.severities[severity] = true
			}
		}
	}
	return filter
}

// apply keeps the findings that match. A query that names the finding — its
// id, its title, one of its packages — keeps it whole; one that names a
// space or an app keeps the finding with only those apps, since that is the
// question it asks.
func (f findingFilter) apply(findings []FindingGroup) []FindingGroup {
	kept := []FindingGroup{}
	for _, finding := range findings {
		if len(f.severities) > 0 && !f.severities[finding.Severity] {
			continue
		}
		if f.fixable && !finding.Fixable {
			continue
		}
		if f.query == "" || f.namesFinding(finding) {
			kept = append(kept, finding)
			continue
		}
		var apps []AffectedApp
		for _, app := range finding.Apps {
			if strings.Contains(strings.ToLower(app.Space+"/"+app.App), f.query) {
				apps = append(apps, app)
			}
		}
		if len(apps) > 0 {
			finding.Apps = apps
			kept = append(kept, finding)
		}
	}
	return kept
}

func (f findingFilter) namesFinding(finding FindingGroup) bool {
	if strings.Contains(strings.ToLower(finding.ID), f.query) || strings.Contains(strings.ToLower(finding.Title), f.query) {
		return true
	}
	for _, app := range finding.Apps {
		for _, pkg := range app.Packages {
			if strings.Contains(strings.ToLower(pkg.Name), f.query) {
				return true
			}
		}
	}
	return false
}

var findingsCSVHeader = []string{
	"kind", "id", "severity", "score", "title", "space", "app", "exposure", "url",
	"package", "ecosystem", "installed", "fixed", "location", "state", "note",
	"space_admins", "space_developers", "scanned_at", "published", "reference",
}

// findingRows is the report as one row per finding, app and package (or,
// for a credential, the file it is in).
func findingRows(report *VulnerabilityReport) [][]string {
	apps := make(map[runtime.AppRef]ReportApp, len(report.Apps))
	for _, app := range report.Apps {
		apps[runtime.AppRef{Space: app.Space, App: app.App}] = app
	}
	var rows [][]string
	for _, finding := range report.Findings {
		score := ""
		if finding.Score > 0 {
			score = strconv.FormatFloat(finding.Score, 'f', 1, 64)
		}
		for _, affected := range finding.Apps {
			app := apps[runtime.AppRef{Space: affected.Space, App: affected.App}]
			contacts := report.Spaces[affected.Space]
			row := func(pkg AffectedPackage, location string) {
				if location == "" {
					location = pkg.Path
				}
				rows = append(rows, []string{
					finding.Kind, finding.ID, affected.Severity, score, finding.Title,
					affected.Space, affected.App, app.Exposure, app.URL,
					pkg.Name, pkg.Type, pkg.Installed, pkg.Fixed, location, affected.State, affected.Note,
					contactList(contacts.Admins), contactList(contacts.Developers),
					app.ScannedAt, finding.Published, finding.URL,
				})
			}
			for _, pkg := range affected.Packages {
				row(pkg, "")
			}
			for _, location := range affected.Locations {
				row(AffectedPackage{}, location)
			}
			if len(affected.Packages)+len(affected.Locations) == 0 {
				row(AffectedPackage{}, "")
			}
		}
	}
	return rows
}

// handleVulnerabilities: GET /v1/vulnerabilities — on donkey-orchestrator,
// which holds the scans. ?severity=, ?q= and ?fixable=true narrow it;
// ?format=csv answers with a download.
func (s *Server) handleVulnerabilities(w http.ResponseWriter, r *http.Request) {
	asCSV, err := reportFormat(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inventory, err := s.currentInventory(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	report := s.vulnerabilityReport(r.Context(), inventory, time.Now())
	report.Findings = parseFindingFilter(r.URL.Query()).apply(report.Findings)
	if asCSV {
		writeCSV(w, r, "vulnerabilities", findingsCSVHeader, findingRows(report))
		return
	}
	writeReport(w, r, report)
}
