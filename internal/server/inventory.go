package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/deployment"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// The platform's reports — which apps a vulnerability is in, what the fleet
// is built with — are read off records donkey already keeps: every running
// app's stored scan, and the release that built its image. Nothing is
// scanned or analyzed for a report; it is the same findings and the same
// detection an app's own page shows, across every app at once.
//
// Those records live on the orchestrator's data volume, so the reports are
// made there. Reading them all is a pass over the whole volume (a scan
// record carries its image's inventory), so what a report needs of each app
// is kept in memory and read again only when the app runs another image or
// its scan was refreshed.

const (
	// One inventory serves every report asked for in the same moment: the
	// page, and the export of what it shows.
	inventoryCacheTTL = 15 * time.Second
	inventoryTimeout  = 2 * time.Minute
	// An app whose records are missing is looked at again after this long.
	// A deploy or a rescan in this process forgets its facts right away
	// (forgetFacts); this covers records another writer left.
	factsRetry = 10 * time.Minute
	// Status reads run side by side where the runtime has no bulk read.
	statusWorkers = 8
)

// appFacts is what the reports know about one running app's image.
type appFacts struct {
	image string
	// stamp is the version of the stored scan the findings were read from;
	// zero when they came with the release record, or there are none.
	stamp deployment.Stamp
	// scan is the image's findings without its inventory; nil when no scan
	// is on record.
	scan *AppScan
	// build is what the build of the image detected; nil without a record.
	build *PlanBuild
	// problem says why a record that exists could not be read.
	problem string
	checked time.Time
}

func (f *appFacts) complete() bool { return f.scan != nil && f.build != nil }

// factsCache keeps each app's facts between reports.
type factsCache struct {
	mu   sync.Mutex
	apps map[runtime.AppRef]*appFacts
}

func (c *factsCache) get(ref runtime.AppRef) *appFacts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apps[ref]
}

func (c *factsCache) put(ref runtime.AppRef, facts *appFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apps == nil {
		c.apps = map[runtime.AppRef]*appFacts{}
	}
	c.apps[ref] = facts
}

// keep drops the facts of apps that no longer run.
func (c *factsCache) keep(running map[runtime.AppRef]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ref := range c.apps {
		if !running[ref] {
			delete(c.apps, ref)
		}
	}
}

// forgetFacts makes the next report read an app's records again: called
// where this process writes them, so a report never trails a deploy or a
// rescan by more than the inventory's own moment.
func (s *Server) forgetFacts(space, app string) {
	s.facts.mu.Lock()
	delete(s.facts.apps, runtime.AppRef{Space: space, App: app})
	s.facts.mu.Unlock()
	s.inventory.forget()
}

// releaseFacts is the part of a release record the reports read.
type releaseFacts struct {
	Plan *struct {
		Build PlanBuild `json:"build"`
	} `json:"plan"`
	Update struct {
		BaseImages []string `json:"baseImages"`
	} `json:"update"`
	Scan *AppScan `json:"scan"`
}

// detected says whether a build record says anything about what the app is
// made of. A redeploy of an existing image records a build that detected
// nothing; the build that made the image is further back.
func detected(b PlanBuild) bool {
	return len(b.Runtimes)+len(b.Frameworks)+len(b.Versions)+len(b.PackageManagers)+len(b.BaseImages) > 0
}

// lean drops what a report never shows, so the facts of a few hundred apps
// stay small: the inventory, the prose, and the masked line of each secret.
func lean(scan *AppScan) *AppScan {
	out := *scan
	out.SBOM, out.Findings = "", nil
	out.Vulnerabilities = append([]build.Vulnerability{}, scan.Vulnerabilities...)
	for i := range out.Vulnerabilities {
		out.Vulnerabilities[i].Description = ""
	}
	out.Secrets = append([]build.Secret{}, scan.Secrets...)
	for i := range out.Secrets {
		out.Secrets[i].Match = ""
	}
	return &out
}

// appFacts reads what the reports need about the image an app runs, or
// returns what it read before while that still holds.
func (s *Server) appFacts(ctx context.Context, ref runtime.AppRef, image string) *appFacts {
	store := s.deploymentStore()
	stamp, stampErr := store.ScanStamp(ref.Space, ref.App, image)
	known := s.facts.get(ref)
	if known != nil && known.image == image && known.stamp.Is(stamp) && stampErr == nil &&
		(known.complete() || time.Since(known.checked) < factsRetry) {
		return known
	}

	facts := &appFacts{image: image, stamp: stamp, checked: time.Now()}
	if stampErr != nil {
		facts.problem = fmt.Sprintf("its stored scan could not be read: %v", stampErr)
	}
	if !stamp.IsZero() {
		// The latest check of the running image: a rescan rewrites this one.
		var record *AppScan
		raw, err := store.Scan(ctx, ref.Space, ref.App, image)
		if err == nil {
			err = json.Unmarshal(raw, &record)
		}
		if err == nil {
			err = record.validate()
		}
		if err != nil {
			facts.problem = fmt.Sprintf("its stored scan could not be read: %v", err)
		} else {
			facts.scan = lean(record)
		}
	}
	if known != nil && known.image == image {
		// The image's build never changes; only its scan is refreshed.
		facts.build = known.build
	}
	if facts.scan == nil || facts.build == nil {
		records, err := store.BuildsByImage(ctx, ref.Space, ref.App, image)
		if err != nil && facts.problem == "" {
			facts.problem = fmt.Sprintf("its build history could not be read: %v", err)
		}
		for _, raw := range records {
			var release releaseFacts
			if json.Unmarshal(raw, &release) != nil {
				continue
			}
			if facts.scan == nil && release.Scan != nil && release.Scan.validate() == nil {
				// Not picked up by the periodic check yet: the build's own scan.
				facts.scan = lean(release.Scan)
			}
			if facts.build == nil && release.Plan != nil {
				found := release.Plan.Build
				if len(release.Update.BaseImages) > 0 {
					// What the build resolved beats what the plan expected.
					found.BaseImages = release.Update.BaseImages
				}
				if detected(found) {
					facts.build = &found
				}
			}
		}
	}
	s.facts.put(ref, facts)
	return facts
}

// inventoryApp is one running app with what its records say.
type inventoryApp struct {
	runtime.AppRef
	Status runtime.AppStatus
	Facts  *appFacts
	// Problem says why the app's state could not be read at all.
	Problem string
}

// fleetInventory is every running app, and the settings of their spaces.
type fleetInventory struct {
	At     time.Time
	Apps   []inventoryApp
	Spaces map[string]config.SpaceConfig
}

// currentInventory is the inventory the reports are made from, read at most
// once per moment however many are asked for.
func (s *Server) currentInventory(ctx context.Context) (*fleetInventory, error) {
	return s.inventory.get(inventoryCacheTTL, func() (*fleetInventory, error) {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryTimeout)
		defer cancel()
		return s.readInventory(readCtx)
	})
}

func (s *Server) readInventory(ctx context.Context) (*fleetInventory, error) {
	apps, err := s.runningApps(ctx)
	if err != nil {
		return nil, err
	}
	spaces, err := s.listSpaceConfigs(ctx)
	if err != nil {
		return nil, err
	}
	running := make(map[runtime.AppRef]bool, len(apps))
	for i := range apps {
		app := &apps[i]
		running[app.AppRef] = true
		if app.Problem == "" && app.Status.Image != "" {
			app.Facts = s.appFacts(ctx, app.AppRef, app.Status.Image)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	s.facts.keep(running)
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Space != apps[j].Space {
			return apps[i].Space < apps[j].Space
		}
		return apps[i].App < apps[j].App
	})
	return &fleetInventory{At: time.Now(), Apps: apps, Spaces: spaces}, nil
}

// runningApps is every deployed app with its status: one bulk read where
// the runtime has one, a status read per app otherwise.
func (s *Server) runningApps(ctx context.Context) ([]inventoryApp, error) {
	if provider, ok := s.rt.(runtime.AppSummaryProvider); ok {
		summaries, err := provider.AppSummaries(ctx)
		if err != nil {
			return nil, err
		}
		apps := make([]inventoryApp, 0, len(summaries))
		for _, summary := range summaries {
			if summary.Status.Found {
				apps = append(apps, inventoryApp{AppRef: summary.AppRef, Status: summary.Status})
			}
		}
		return apps, nil
	}
	refs, err := s.rt.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	read := make([]inventoryApp, len(refs))
	var wg sync.WaitGroup
	slots := make(chan struct{}, statusWorkers)
	for i, ref := range refs {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			read[i] = inventoryApp{AppRef: ref}
			status, err := s.rt.Status(ctx, ref.Space, ref.App)
			switch {
			case err != nil:
				// One app the runtime cannot describe must not cost the
				// report every other app; it is listed, and says so.
				log.Printf("inventory app=%s space=%s: %v", ref.App, ref.Space, err)
				read[i].Problem = "its state could not be read"
			case status != nil:
				read[i].Status = *status
			}
		})
	}
	wg.Wait()
	apps := read[:0]
	for _, app := range read {
		if app.Problem != "" || app.Status.Found {
			apps = append(apps, app)
		}
	}
	return apps, nil
}

// SpaceContacts is who answers for a space: the people and groups holding a
// role that can change what runs in it.
type SpaceContacts struct {
	Admins     config.AccessConfig `json:"admins"`
	Developers config.AccessConfig `json:"developers"`
}

// ReportApp is one running app as a report names it.
type ReportApp struct {
	Space    string `json:"space"`
	App      string `json:"app"`
	URL      string `json:"url,omitempty"`
	Exposure string `json:"exposure,omitempty"`
	Stack    string `json:"stack,omitempty"`
	Image    string `json:"image,omitempty"`
	// Scanned says a scan of the running image is on record. The times and
	// the operating system are that scan's.
	Scanned     bool   `json:"scanned"`
	ScannedAt   string `json:"scannedAt,omitempty"`
	DBUpdatedAt string `json:"dbUpdatedAt,omitempty"`
	OS          string `json:"os,omitempty"`
	EndOfLife   bool   `json:"endOfLife,omitempty"`
	// Security is the scan's outcome as the running app carries it.
	Security *AppSecurity `json:"security,omitempty"`
	// Problem says why the app's records could not be read.
	Problem string `json:"problem,omitempty"`
}

// reportApps lists the inventory's apps the way every report does.
func (s *Server) reportApps(inventory *fleetInventory) []ReportApp {
	apps := make([]ReportApp, 0, len(inventory.Apps))
	for _, app := range inventory.Apps {
		entry := ReportApp{
			Space: app.Space, App: app.App, Stack: app.Status.Stack, Image: app.Status.Image,
			Security: parseSecurity(app.Status.Security), Problem: app.Problem,
		}
		if app.Status.Found {
			entry.Exposure = exposureOf(&app.Status)
		}
		if app.Status.PublicHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + app.Status.PublicHost
		}
		if facts := app.Facts; facts != nil {
			if entry.Problem == "" {
				entry.Problem = facts.problem
			}
			if scan := facts.scan; scan != nil {
				entry.Scanned = true
				entry.ScannedAt, entry.DBUpdatedAt = scan.ScannedAt, scan.DBUpdatedAt
				entry.OS, entry.EndOfLife = scan.OS, scan.EndOfLife
			}
		}
		apps = append(apps, entry)
	}
	return apps
}

// reportContacts names who answers for each space that has an app.
func reportContacts(inventory *fleetInventory) map[string]SpaceContacts {
	contacts := map[string]SpaceContacts{}
	for _, app := range inventory.Apps {
		if _, ok := contacts[app.Space]; !ok {
			settings := inventory.Spaces[app.Space]
			contacts[app.Space] = SpaceContacts{Admins: settings.Admins, Developers: settings.Developers}
		}
	}
	return contacts
}

// platformAdmins is who runs the installation: the contact of last resort
// for a space nobody was given.
func platformAdmins(ctx context.Context) config.AccessConfig {
	settings, _ := ctx.Value(platformContextKey{}).(config.PlatformConfig)
	return settings.Admins
}
