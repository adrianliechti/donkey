package server

import (
	"io/fs"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	Healthy bool   `json:"healthy"`
	Ready   string `json:"ready"`
	Image   string `json:"image"`
	URL     string `json:"url"`
}

// handleListApps finds every donkey-managed app by its namespace labels.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	nsList, err := s.k8s.Clientset.CoreV1().Namespaces().List(r.Context(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=donkey,donkey.dev/app",
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	apps := []AppListEntry{}
	for _, ns := range nsList.Items {
		name := ns.Labels["donkey.dev/app"]
		if name == "" || name == "donkey-system" {
			continue
		}
		st, err := s.k8s.AppStatus(r.Context(), ns.Name, name)
		if err != nil || !st.Found {
			continue
		}
		entry := AppListEntry{
			Name:    name,
			Healthy: st.Healthy(),
			Ready:   st.ReadyText(),
			Image:   st.Image,
		}
		if st.IngressHost != "" {
			entry.URL = s.cfg.Scheme() + "://" + st.IngressHost
		}
		apps = append(apps, entry)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}
