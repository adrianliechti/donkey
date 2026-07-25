package server

import (
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

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
		// The console holds an API token in localStorage, so script injection
		// or framing would hand over the whole control plane. The bundle is
		// self-contained (no CDNs), so a strict policy costs nothing —
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
			Name:     ref.App,
			Space:    space,
			Healthy:  st.Healthy(),
			Ready:    st.ReadyText(),
			Image:    st.Image,
			Restarts: st.Restarts,
		}
		if !st.StartedAt.IsZero() {
			entry.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
		if st.IngressHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + st.IngressHost
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
