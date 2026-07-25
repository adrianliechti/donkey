package server

import (
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"monobox.ch/donkey/web"
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
			http.NotFound(w, r)
			return
		}
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
	Name    string `json:"name"`
	Space   string `json:"space"`
	Healthy bool   `json:"healthy"`
	Ready   string `json:"ready"`
	Image   string `json:"image"`
	URL     string `json:"url"`
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
		st, err := s.k8s.AppStatus(r.Context(), ref.Namespace, ref.App)
		if err != nil || !st.Found {
			continue
		}
		space := ref.Space
		if space == "" {
			space = strings.TrimPrefix(ref.Namespace, s.cfg.NamespacePrefix)
		}
		entry := AppListEntry{
			Name:    ref.App,
			Space:   space,
			Healthy: st.Healthy(),
			Ready:   st.ReadyText(),
			Image:   st.Image,
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
