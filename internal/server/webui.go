package server

import (
	"context"
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
	settings, err := s.listSpaceConfigs(r.Context())
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.spaceList(r.Context(), settings))
}

type spaceListResponse struct {
	Spaces        []string            `json:"spaces"`
	Roles         map[string]string   `json:"roles"`
	Domains       map[string][]string `json:"domains"`
	PlatformAdmin bool                `json:"platformAdmin"`
	Domain        string              `json:"domain"`
	AllowPublic   map[string]bool     `json:"allowPublic"`
}

func (s *Server) spaceList(ctx context.Context, settings map[string]config.SpaceConfig) spaceListResponse {
	names := []string{}
	roles := map[string]string{}
	domains := map[string][]string{}
	public := map[string]bool{}
	for space, policy := range settings {
		role := policy.Role(requestIdentity(ctx))
		if s.platformAdmin(ctx) {
			role = config.SpaceAdmin
		}
		if role != "" {
			names = append(names, space)
			roles[space] = role
			domains[space] = append([]string{}, policy.Domains...)
			public[space] = policy.AllowPublic
		}
	}
	sort.Strings(names)
	return spaceListResponse{
		Spaces: names, Roles: roles, Domains: domains,
		PlatformAdmin: s.platformAdmin(ctx), Domain: s.cfg.Domain, AllowPublic: public,
	}
}

// The portal reads apps and spaces together, sharing one fresh permission
// snapshot and one authentication check instead of making two HTTP requests.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.listSpaceConfigs(r.Context())
	if err != nil {
		writeAccessError(w, err)
		return
	}
	ctx := context.WithValue(r.Context(), spaceConfigsContextKey{}, settings)
	apps, err := s.listAppEntries(ctx)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Apps []AppListEntry `json:"apps"`
		spaceListResponse
	}{apps, s.spaceList(ctx, settings)})
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
	if err := s.validateSpaceOfferings(r.Context(), &req.SpaceConfig, config.SpaceConfig{}); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	mu := s.spaceLock(req.Name)
	mu.Lock()
	defer mu.Unlock()
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
// bindings, managed services, disks, runtime space, retained telemetry and
// source history.
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
	// The space's source history goes with it, so a new space by the same
	// name starts with none of the old one's code.
	if s.history != nil {
		if err := s.history.RemoveSpace(space); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
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
	apps, err := s.listAppEntries(r.Context())
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

func (s *Server) listAppEntries(ctx context.Context) ([]AppListEntry, error) {
	var refs []runtime.AppRef
	var summaries map[runtime.AppRef]runtime.AppSummary
	if provider, ok := s.rt.(runtime.AppSummaryProvider); ok {
		apps, err := provider.AppSummaries(ctx)
		if err != nil {
			return nil, err
		}
		summaries = make(map[runtime.AppRef]runtime.AppSummary, len(apps))
		for _, app := range apps {
			refs = append(refs, app.AppRef)
			summaries[app.AppRef] = app
		}
		if _, loaded := ctx.Value(spaceConfigsContextKey{}).(map[string]config.SpaceConfig); !loaded && !s.platformAdmin(ctx) {
			settings, err := s.listSpaceConfigs(ctx)
			if err != nil {
				return nil, err
			}
			ctx = context.WithValue(ctx, spaceConfigsContextKey{}, settings)
		}
	} else {
		var err error
		refs, err = s.rt.ListApps(ctx)
		if err != nil {
			return nil, err
		}
	}
	apps := []AppListEntry{}
	roles := map[string]string{}
	for _, ref := range refs {
		role, checked := roles[ref.Space]
		if !checked {
			var err error
			role, err = s.spaceRole(ctx, ref.Space)
			if err != nil {
				return nil, err
			}
			roles[ref.Space] = role
		}
		if role != config.SpaceAdmin && role != config.SpaceDeveloper && role != config.SpaceViewer {
			continue
		}
		var st *runtime.AppStatus
		var hasStorage bool
		var addonNames []string
		if summary, ok := summaries[ref]; ok {
			st, hasStorage, addonNames = &summary.Status, summary.HasStorage, summary.Addons
		} else {
			var err error
			st, err = s.rt.Status(ctx, ref.Space, ref.App)
			if err != nil {
				// A transient status error must not make a running app vanish.
				apps = append(apps, AppListEntry{Name: ref.App, Space: ref.Space, Ready: "status unavailable"})
				continue
			}
			if role != config.SpaceViewer {
				hasStorage = s.rt.HasData(ctx, ref.Space, ref.App)
				if bindings, err := s.rt.ListServices(ctx, ref.Space, ref.App); err == nil {
					for _, b := range bindings {
						addonNames = append(addonNames, b.Name)
					}
				}
			}
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
			HasStorage: hasStorage,
			Addons:     addonNames,
		}
		if !st.StartedAt.IsZero() {
			entry.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
		if st.PublicHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + st.PublicHost
		}
		if role == config.SpaceViewer {
			entry.Image, entry.Addons, entry.HasStorage = "", nil, false
		}
		apps = append(apps, entry)
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Space != apps[j].Space {
			return apps[i].Space < apps[j].Space
		}
		return apps[i].Name < apps[j].Name
	})
	return apps, nil
}
