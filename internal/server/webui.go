package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
	"github.com/adrianliechti/donkey/web"
)

// uiHandler serves the embedded admin console: real files as-is, everything
// else falls back to index.html (the SPA handles its own views).
func uiHandler() http.Handler {
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // embed is broken at build time, not at runtime
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The console acts with the gateway's session cookie, so script
		// injection or framing would act as the signed-in admin. The bundle
		// is self-contained (no CDNs), so a strict policy costs nothing —
		// 'unsafe-inline' covers only the stylesheet Tailwind injects.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; font-src 'self' data:; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Apps and the console are https-only; the http listener just redirects.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if f, err := sub.Open(path); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, sub, "index.html")
	})
}

// handleListSpaces names every space, with or without apps — a space that
// holds only services still needs its card in the console.
func (s *Server) handleListSpaces(w http.ResponseWriter, r *http.Request) {
	refs, err := s.k8s.ListSpaces(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	names := []string{}
	for _, ref := range refs {
		space := ref.Space
		if space == "" {
			space = strings.TrimPrefix(ref.Namespace, s.cfg.NamespacePrefix)
		}
		names = append(names, space)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"spaces": names})
}

// handleCreateSpace makes an empty space — a home to put services into
// before any app arrives.
func (s *Server) handleCreateSpace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ns, err := s.cfg.SpaceNamespace(req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.k8s.CheckNamespaceFree(r.Context(), ns); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err := s.k8s.Apply(r.Context(), []runtime.Object{manifests.Namespace(req.Name, ns)}); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

// handleDeleteSpace removes a space that holds no apps — the console made
// it, the console can unmake it. Its services and their data go with it.
func (s *Server) handleDeleteSpace(w http.ResponseWriter, r *http.Request) {
	ns, err := s.cfg.SpaceNamespace(r.PathValue("space"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.k8s.DeleteSpace(r.Context(), ns); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// AppListEntry is one row of the admin console's overview.
type AppListEntry struct {
	Name     string `json:"name"`
	Space    string `json:"space"`
	Healthy  bool   `json:"healthy"`
	Ready    string `json:"ready"`
	Image    string `json:"image"`
	URL      string `json:"url"`
	Restarts int32  `json:"restarts,omitempty"`
	// StartedAt is when the newest pod came up (RFC 3339; empty when none).
	StartedAt string `json:"startedAt,omitempty"`
	// Exposure (public | protected | internal), disk and attached services —
	// what the console's canvas cards show without a per-app request.
	Exposure   string   `json:"exposure,omitempty"`
	HasStorage bool     `json:"hasStorage,omitempty"`
	Addons     []string `json:"addons,omitempty"`
}

// handleListApps finds every deployed app, with the space it belongs to. The
// console groups by space; the CLI prints the same grouping.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	refs, err := s.k8s.ListApps(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	apps := []AppListEntry{}
	for _, ref := range refs {
		space := ref.Space
		if space == "" {
			space = strings.TrimPrefix(ref.Namespace, s.cfg.NamespacePrefix)
		}
		st, err := s.k8s.AppStatus(r.Context(), ref.Namespace, ref.App)
		if err != nil {
			// A transient status error must not make a running app vanish
			// from the overview.
			apps = append(apps, AppListEntry{Name: ref.App, Space: space, Ready: "status unavailable"})
			continue
		}
		if !st.Found {
			continue
		}
		entry := AppListEntry{
			Name:       ref.App,
			Space:      space,
			Healthy:    st.Healthy(),
			Ready:      st.ReadyText(),
			Image:      st.Image,
			Restarts:   st.Restarts,
			Exposure:   exposureOf(st),
			HasStorage: s.k8s.HasDataClaim(r.Context(), ref.Namespace, ref.App),
		}
		if !st.StartedAt.IsZero() {
			entry.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
		if st.IngressHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + st.IngressHost
		}
		if bindings, err := s.k8s.ListAddonBindings(r.Context(), ref.Namespace, ref.App); err == nil {
			for _, b := range bindings {
				entry.Addons = append(entry.Addons, b.Name)
			}
		}
		apps = append(apps, entry)
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Space != apps[j].Space {
			return apps[i].Space < apps[j].Space
		}
		return apps[i].Name < apps[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}
