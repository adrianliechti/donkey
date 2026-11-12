// Package docker runs donkey's apps straight on a Docker engine — no
// Kubernetes, no kind. Spaces are user-defined networks (apps of a space
// reach each other by plain container DNS), apps and provisioned services
// are containers with donkey labels, disks are named volumes, and the small
// records Kubernetes keeps in Secrets and ConfigMaps (env, bindings, route
// table entries, app metadata) live as YAML files in a state directory — a
// named volume shared with the donkey gateway, whose routing table is
// exactly the routes/ folder (and whose issued certificates live next to it).
package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
)

// Route has the same fields as Kubernetes route configuration. Docker stores
// it as a YAML record.
type Route = config.GatewayRoute

// A record is one file, <name>.yaml. Older installations wrote <name>.json;
// those are still read, and the next write of a record replaces its JSON, so
// an upgrade needs no migration step.
const (
	recordExt       = ".yaml"
	legacyRecordExt = ".json"
)

// state reads and writes the provider's file records. Every method takes
// donkey names (space, app) and record names without an extension — the
// layout underneath is nobody's business.
type state struct{ dir string }

func (s state) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

// recordName is the record a file holds, whichever format it is in.
func recordName(file string) (string, bool) {
	if name, ok := strings.CutSuffix(file, recordExt); ok {
		return name, true
	}
	return strings.CutSuffix(file, legacyRecordExt)
}

// readRecordFile reads the record at base (a path without extension): the
// YAML, else the legacy JSON. A writer may replace the one with the other
// between the two reads, so a missing JSON gets the YAML a second look.
func readRecordFile(base string) ([]byte, error) {
	b, err := os.ReadFile(base + recordExt)
	if !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	if b, err = os.ReadFile(base + legacyRecordExt); !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	return os.ReadFile(base + recordExt)
}

// ReadRecordFile reads a record by file path, for readers outside the
// provider (the service proxy's configuration): a path ending in .yaml or
// .json names the record, read with the same YAML-first fallback. Any other
// path is read as it is.
func ReadRecordFile(path string) ([]byte, error) {
	if base, ok := recordName(path); ok {
		return readRecordFile(base)
	}
	return os.ReadFile(path)
}

func (s state) readRecord(target any, parts ...string) (bool, error) {
	b, err := readRecordFile(s.path(parts...))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, yaml.Unmarshal(b, target)
}

// readMap reads a string map record; a missing or emptied one is empty.
func (s state) readMap(parts ...string) (map[string]string, error) {
	m := map[string]string{}
	_, err := s.readRecord(&m, parts...)
	if m == nil {
		m = map[string]string{}
	}
	return m, err
}

func (s state) writeRecord(value any, parts ...string) error {
	return s.storeRecord(value, true, parts...)
}

func (s state) storeRecord(value any, replace bool, parts ...string) error {
	base := s.path(parts...)
	p := base + recordExt
	if !replace {
		// A legacy JSON record exists just as much as a YAML one.
		switch _, err := os.Lstat(base + legacyRecordExt); {
		case err == nil:
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	// Write-then-rename: the gateway reads these files concurrently and must
	// never see half a route.
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	mode := fs.FileMode(0o644)
	// Run records carry donkey.yaml's env, so they are as private as env.
	if len(parts) > 0 && (parts[0] == "env" || parts[0] == "bindings" || parts[0] == "provisioning" || parts[0] == "deploy") {
		mode = 0o600
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if replace {
		if err := os.Rename(tmp.Name(), p); err != nil {
			return err
		}
		// Only once the YAML is complete does the legacy JSON go; readers
		// prefer the YAML, so the two never disagree in between.
		return removeFile(base + legacyRecordExt)
	}
	// A hard link publishes the complete record only if it does not exist.
	// Competing first sign-ins can never overwrite the first saved admin.
	err = os.Link(tmp.Name(), p)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

// remove deletes a record in both formats, the legacy one first so that an
// interrupted removal never leaves an older JSON to resurface.
func (s state) remove(parts ...string) error {
	base := s.path(parts...)
	if err := removeFile(base + legacyRecordExt); err != nil {
		return err
	}
	return removeFile(base + recordExt)
}

func removeFile(p string) error {
	err := os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// exists reports whether a record is stored, in either format.
func (s state) exists(parts ...string) (bool, error) {
	base := s.path(parts...)
	for _, ext := range []string{recordExt, legacyRecordExt} {
		_, err := os.Stat(base + ext)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// records names the records of one folder; a missing folder has none.
func (s state) records(parts ...string) ([]string, error) {
	entries, err := os.ReadDir(s.path(parts...))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return recordNames(entries), nil
}

// recordNames names each record of a folder listing once, sorted. A record
// found in both formats (an interrupted rewrite) is still one record, and
// readRecord reads its YAML.
func recordNames(entries []os.DirEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name, ok := recordName(e.Name())
		if !ok || name == "" || e.IsDir() || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// --- env ------------------------------------------------------------------

// env is what `donkey env` stores — the app's env Secret on Kubernetes.
// donkey.yaml's env belongs to the deploy instead (runSpec.Env).
func (s state) env(space, app string) (map[string]string, error) {
	return s.readMap("env", space, app)
}

func (s state) setEnv(space, app string, env map[string]string) error {
	return s.writeRecord(env, "env", space, app)
}

// --- app metadata ---------------------------------------------------------

func (s state) meta(space, app string) (map[string]string, error) {
	return s.readMap("meta", space, app)
}

func (s state) setMeta(space, app string, m map[string]string) error {
	return s.writeRecord(m, "meta", space, app)
}

// --- bindings -------------------------------------------------------------

// bindingName is one owner's copy of a service binding: the space's own
// (record <name>, owner empty) or an app's attachment (<app>.<name>). The
// dot separates safely — donkey names never contain one.
func bindingName(owner, name string) string {
	if owner == sharedOwner {
		return name
	}
	return owner + "." + name
}

// legacySharedBindingPrefix is how the space's own bindings used to be
// filed, under a pseudo-app called "shared". Only migration spells it.
const legacySharedBindingPrefix = "shared."

// migrateSharedBindings moves a space's own bindings from the pseudo-app
// layout onto plain <name>. A record already at the new name wins.
func (s state) migrateSharedBindings(space string) error {
	entries, err := os.ReadDir(s.path("bindings", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		// The pseudo-app layout predates YAML records: it is JSON files only,
		// and a shared.<name>.yaml is the copy of an app called "shared".
		rest, ok := strings.CutPrefix(e.Name(), legacySharedBindingPrefix)
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, legacyRecordExt)
		if !ok || name == "" || strings.Contains(name, ".") {
			continue
		}
		taken, err := s.exists("bindings", space, name)
		if err != nil {
			return err
		}
		if taken {
			if err := os.Remove(s.path("bindings", space, e.Name())); err != nil {
				return err
			}
			continue
		}
		// Moved as it is: still a legacy JSON, read until its next write.
		if err := os.Rename(s.path("bindings", space, e.Name()), s.path("bindings", space, rest)); err != nil {
			return err
		}
	}
	return nil
}

func (s state) binding(space, owner, name string) (map[string]string, error) {
	return s.readMap("bindings", space, bindingName(owner, name))
}

func (s state) setBinding(space, owner, name string, data map[string]string) error {
	return s.writeRecord(data, "bindings", space, bindingName(owner, name))
}

func (s state) removeBinding(space, owner, name string) error {
	return s.remove("bindings", space, bindingName(owner, name))
}

// listBindings names the services an owner holds a binding of.
func (s state) listBindings(space, owner string) ([]string, error) {
	names, err := s.records("bindings", space)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rest := range names {
		if owner != sharedOwner {
			var ok bool
			if rest, ok = strings.CutPrefix(rest, owner+"."); !ok {
				continue
			}
		}
		if rest != "" && !strings.Contains(rest, ".") {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out, nil
}

// attachments names the apps holding a copy of one service's binding.
func (s state) attachments(space, name string) ([]string, error) {
	names, err := s.records("bindings", space)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, record := range names {
		owner, ok := strings.CutSuffix(record, "."+name)
		if ok && owner != "" && !strings.Contains(owner, ".") {
			out = append(out, owner)
		}
	}
	sort.Strings(out)
	return out, nil
}

// bindingValues collects one key's values across every space's shared
// bindings — the AI proxy's token list.
func (s state) bindingValues(key, matchKey, matchValue string) ([]string, error) {
	var out []string
	spaces, err := os.ReadDir(s.path("bindings"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, sp := range spaces {
		names, err := s.records("bindings", sp.Name())
		if err != nil {
			continue
		}
		for _, name := range names {
			// The space's own bindings are the records with no owner prefix.
			if strings.Contains(name, ".") {
				continue
			}
			if data, err := s.readMap("bindings", sp.Name(), name); err == nil {
				if v := data[key]; v != "" && data[matchKey] == matchValue {
					out = append(out, v)
				}
			}
		}
	}
	return out, nil
}

// --- routes ---------------------------------------------------------------

func (s state) route(space, name string) (*Route, error) {
	var r Route
	found, err := s.readRecord(&r, "routes", space, name)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s state) setRoute(space, name string, r Route) error {
	return s.writeRecord(r, "routes", space, name)
}

func (s state) removeRoute(space, name string) error {
	return s.remove("routes", space, name)
}

// listRoutes names a space's routes.
func (s state) listRoutes(space string) ([]string, error) {
	return s.records("routes", space)
}

// ReadRoutes is the gateway's view: every route of every space, plus the
// system routes (the console's). It is exported for the gateway's file
// route source.
func ReadRoutes(stateDir string) ([]Route, error) {
	s := state{dir: stateDir}
	spaces, err := os.ReadDir(s.path("routes"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Route
	for _, sp := range spaces {
		names, err := s.listRoutes(sp.Name())
		if err != nil {
			continue
		}
		for _, name := range names {
			if r, err := s.route(sp.Name(), name); err == nil && r != nil {
				out = append(out, *r)
			}
		}
	}
	return out, nil
}

// WriteSystemRoute records a route donkey's installer owns (the console).
func WriteSystemRoute(stateDir, name string, r Route) error {
	return state{dir: stateDir}.setRoute("system", name, r)
}

// cleanupApp removes the records of one app. keepData spares its env and its
// binding copies, as Kubernetes spares the Secrets: a kept disk is worthless
// without the credentials and settings it was used with. Metadata and routes
// go either way, with the Deployment and route ConfigMaps they stand for.
func (s state) cleanupApp(space, app string, keepData bool) error {
	if !keepData {
		if err := s.remove("env", space, app); err != nil {
			return err
		}
		names, err := s.listBindings(space, app)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := s.removeBinding(space, app, name); err != nil {
				return err
			}
		}
	}
	if err := s.remove("meta", space, app); err != nil {
		return err
	}
	names, err := s.listRoutes(space)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == app || strings.HasPrefix(name, app+".on.") {
			if err := s.removeRoute(space, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupSpace removes a space's records entirely.
func (s state) cleanupSpace(space string) error {
	for _, sub := range []string{"env", "meta", "bindings", "routes", "deploy", "spaces", "provisioning", "service-metadata"} {
		if err := os.RemoveAll(s.path(sub, space)); err != nil {
			return err
		}
	}
	return nil
}

// pathRouteName names one shared-address route record. The dots separate
// unambiguously — donkey names never contain one — which is what lets
// "every route of app X" be found by the "X.on." prefix.
func pathRouteName(app, targetApp, path string) string {
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(path)), "-")
	if slug == "" {
		slug = "root"
	}
	return fmt.Sprintf("%s.on.%s.%s", app, targetApp, slug)
}
