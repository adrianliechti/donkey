package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
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
	spaces, err := s.rt.ListSpaces(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	names := []string{}
	roles := map[string]string{}
	domains := map[string][]string{}
	for _, space := range spaces {
		role, err := s.spaceRole(r.Context(), space)
		if err != nil {
			writeAccessError(w, err)
			return
		}
		if role != "" {
			settings, err := s.rt.SpaceConfig(r.Context(), space)
			if err != nil {
				writeAccessError(w, err)
				return
			}
			names = append(names, space)
			roles[space] = role
			domains[space] = append([]string{}, settings.Domains...)
		}
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{
		"spaces": names, "roles": roles, "domains": domains,
		"platformAdmin": s.platformAdmin(r.Context()),
		"domain":        s.cfg.Domain,
	})
}

// handleCreateSpace makes an empty space — a home to put services into
// before any app arrives.
func (s *Server) handleCreateSpace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		config.SpaceConfig
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 160<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.rt.ValidateSpace(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Domains == nil {
		req.Domains = config.DefaultSpaceConfig(req.Name, s.cfg.Domain).Domains
	}
	if err := req.SpaceConfig.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.rt.SpaceConfig(r.Context(), req.Name); err == nil {
		writeErr(w, http.StatusConflict, fmt.Errorf("the space already exists"))
		return
	} else if !errors.Is(err, runtime.ErrSpaceNotFound) {
		writeAccessError(w, err)
		return
	}
	if err := s.rt.CheckSpaceFree(r.Context(), req.Name); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err := s.rt.CreateSpace(r.Context(), req.Name); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.rt.SetSpaceConfig(r.Context(), req.Name, req.SpaceConfig); err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

// handleDeleteSpace removes the whole lifecycle boundary: routes, apps,
// bindings, managed services, disks, runtime space, and retained telemetry.
func (s *Server) handleDeleteSpace(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.rt.DeleteSpace(r.Context(), space); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Runtime deletion comes first. If telemetry is temporarily unavailable,
	// retrying this endpoint is safe and completes the purge without reviving
	// any runtime resource.
	if err := s.telemetry.PurgeSpace(r.Context(), space); err != nil {
		writeErr(w, http.StatusBadGateway, err)
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
	// Mode, exposure, disk and attached services —
	// what the console's canvas cards show without a per-app request.
	Exposure   string   `json:"exposure,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	HasStorage bool     `json:"hasStorage,omitempty"`
	Addons     []string `json:"addons,omitempty"`
}

// handleListApps finds every deployed app, with the space it belongs to. The
// console groups by space; the CLI prints the same grouping.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	refs, err := s.rt.ListApps(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	apps := []AppListEntry{}
	roles := map[string]string{}
	for _, ref := range refs {
		role, checked := roles[ref.Space]
		if !checked {
			var err error
			role, err = s.spaceRole(r.Context(), ref.Space)
			if err != nil {
				writeAccessError(w, err)
				return
			}
			roles[ref.Space] = role
		}
		if role != config.SpaceAdmin && role != config.SpaceDeveloper {
			continue
		}
		st, err := s.rt.Status(r.Context(), ref.Space, ref.App)
		if err != nil {
			// A transient status error must not make a running app vanish
			// from the overview.
			apps = append(apps, AppListEntry{Name: ref.App, Space: ref.Space, Ready: "status unavailable"})
			continue
		}
		if !st.Found {
			continue
		}
		entry := AppListEntry{
			Name:       ref.App,
			Space:      ref.Space,
			Healthy:    st.Healthy(),
			Ready:      st.ReadyText(),
			Image:      st.Image,
			Restarts:   st.Restarts,
			Exposure:   exposureOf(st),
			Mode:       modeOfStatus(st),
			HasStorage: s.rt.HasData(r.Context(), ref.Space, ref.App),
		}
		if !st.StartedAt.IsZero() {
			entry.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
		if st.PublicHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + st.PublicHost
		}
		if bindings, err := s.rt.ListServices(r.Context(), ref.Space, ref.App); err == nil {
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
