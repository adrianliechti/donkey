// Package docker runs donkey's apps straight on a Docker engine — no
// Kubernetes, no kind. Spaces are user-defined networks (apps of a space
// reach each other by plain container DNS), apps and provisioned services
// are containers with donkey labels, disks are named volumes, and the small
// records Kubernetes kept in Secrets and annotations (env, bindings, route
// table entries, app metadata) live as files in a state directory — a named
// volume shared with the donkey gateway, whose routing table is exactly the
// routes/ folder (and whose issued certificates live next to it).
package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

// Route is one routing-table entry as the gateway reads it from the state
// directory (mirrors the annotations of the Kubernetes route ConfigMaps).
type Route struct {
	Space       string               `json:"space,omitempty"`
	App         string               `json:"app,omitempty"`
	Host        string               `json:"host"`
	Path        string               `json:"path"`
	StripPrefix bool                 `json:"stripPrefix,omitempty"`
	Backend     string               `json:"backend"`
	Login       bool                 `json:"login,omitempty"`
	Mode        string               `json:"mode,omitempty"`
	CORS        string               `json:"cors,omitempty"`
	Access      *config.AccessConfig `json:"access,omitempty"`
	// Token forwards a sealed credential to the backend — set only on the
	// console route, whose backend verifies identity itself.
	Token bool `json:"token,omitempty"`
}

// state reads and writes the provider's file records. Every method takes
// donkey names (space, app) — the layout underneath is nobody's business.
type state struct{ dir string }

func (s state) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

func (s state) readJSON(target any, parts ...string) (bool, error) {
	b, err := os.ReadFile(s.path(parts...))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, target)
}

func (s state) writeJSON(value any, parts ...string) error {
	return s.storeJSON(value, true, parts...)
}

func (s state) storeJSON(value any, replace bool, parts ...string) error {
	p := s.path(parts...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
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
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if replace {
		return os.Rename(tmp.Name(), p)
	}
	// A hard link publishes the complete record only if it does not exist.
	// Competing first sign-ins can never overwrite the first saved admin.
	err = os.Link(tmp.Name(), p)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

func (s state) remove(parts ...string) error {
	err := os.Remove(s.path(parts...))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// --- env ------------------------------------------------------------------

func (s state) env(space, app string) (map[string]string, error) {
	env := map[string]string{}
	_, err := s.readJSON(&env, "env", space, app+".json")
	return env, err
}

func (s state) setEnv(space, app string, env map[string]string) error {
	return s.writeJSON(env, "env", space, app+".json")
}

// --- app metadata ---------------------------------------------------------

func (s state) meta(space, app string) (map[string]string, error) {
	m := map[string]string{}
	_, err := s.readJSON(&m, "meta", space, app+".json")
	return m, err
}

func (s state) setMeta(space, app string, m map[string]string) error {
	return s.writeJSON(m, "meta", space, app+".json")
}

// --- bindings -------------------------------------------------------------

// bindingName is one owner's copy of a service binding: the space's own
// (owner "shared") or an app's attachment. The dot separates safely — donkey
// names never contain one.
func bindingName(owner, name string) string { return owner + "." + name + ".json" }

func (s state) binding(space, owner, name string) (map[string]string, error) {
	data := map[string]string{}
	_, err := s.readJSON(&data, "bindings", space, bindingName(owner, name))
	return data, err
}

func (s state) setBinding(space, owner, name string, data map[string]string) error {
	return s.writeJSON(data, "bindings", space, bindingName(owner, name))
}

func (s state) removeBinding(space, owner, name string) error {
	return s.remove("bindings", space, bindingName(owner, name))
}

// listBindings names the services an owner holds a binding of.
func (s state) listBindings(space, owner string) ([]string, error) {
	entries, err := os.ReadDir(s.path("bindings", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if rest, ok := strings.CutPrefix(e.Name(), owner+"."); ok {
			if name, ok := strings.CutSuffix(rest, ".json"); ok {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// attachments names the apps holding a copy of one service's binding.
func (s state) attachments(space, name string) ([]string, error) {
	entries, err := os.ReadDir(s.path("bindings", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		owner, ok := strings.CutSuffix(e.Name(), "."+name+".json")
		if ok && !strings.Contains(owner, ".") && owner != sharedOwner {
			out = append(out, owner)
		}
	}
	sort.Strings(out)
	return out, nil
}

// bindingValues collects one key's values across every space's shared
// bindings — the AI proxy's token list.
func (s state) bindingValues(key string) ([]string, error) {
	var out []string
	spaces, err := os.ReadDir(s.path("bindings"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, sp := range spaces {
		entries, err := os.ReadDir(s.path("bindings", sp.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), sharedOwner+".") {
				continue
			}
			data := map[string]string{}
			if _, err := s.readJSON(&data, "bindings", sp.Name(), e.Name()); err == nil {
				if v := data[key]; v != "" {
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
	found, err := s.readJSON(&r, "routes", space, name+".json")
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s state) setRoute(space, name string, r Route) error {
	return s.writeJSON(r, "routes", space, name+".json")
}

func (s state) removeRoute(space, name string) error {
	return s.remove("routes", space, name+".json")
}

// listRoutes names a space's route files, optionally filtered by prefix.
func (s state) listRoutes(space string) ([]string, error) {
	entries, err := os.ReadDir(s.path("routes", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
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

// cleanupApp removes every record of one app (bindings only with the data).
func (s state) cleanupApp(space, app string, keepData bool) error {
	if err := s.remove("env", space, app+".json"); err != nil {
		return err
	}
	if err := s.remove("meta", space, app+".json"); err != nil {
		return err
	}
	if !keepData {
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
	for _, sub := range []string{"env", "meta", "bindings", "routes", "deploy", "spaces"} {
		if err := os.RemoveAll(s.path(sub, space)); err != nil {
			return err
		}
	}
	return nil
}

// pathRouteName names one shared-address route file. The dots separate
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
