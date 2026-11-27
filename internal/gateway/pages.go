package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/adrianliechti/donkey/internal/webpage"
)

// The gateway's own pages wear the console's shell (internal/webpage), so an
// error in front of an app still looks like donkey, in light and dark alike.
func writePage(w http.ResponseWriter, status int, title, detail string) {
	webpage.WriteNotice(w, status, webpage.Notice{Title: title, Detail: detail, Foot: "donkey gateway"})
}

func (s *Server) errorPage(w http.ResponseWriter, status int, title, detail string) {
	writePage(w, status, title, detail)
}

func (s *Server) messagePage(w http.ResponseWriter, status int, title, detail string) {
	writePage(w, status, title, detail)
}

func (s *Server) deniedPage(w http.ResponseWriter, who string) {
	writePage(w, http.StatusForbidden,
		"This app is restricted.",
		fmt.Sprintf("You're signed in as %s, but this app's access list doesn't include you. Ask whoever runs it to add you — or sign out at /oauth2/logout.", who))
}

func maintenanceResponse(w http.ResponseWriter, r *http.Request, rt route) bool {
	if rt.MaintenanceAt == 0 || time.Now().Unix() < rt.MaintenanceAt {
		return false
	}
	const title = "This app needs its owner's attention."
	const detail = "Access is paused until the owner resolves an outstanding issue. If you manage this app, open its space in the Donkey console."
	if wantsHTML(r) {
		writePage(w, http.StatusServiceUnavailable, title, detail)
	} else {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "maintenance_required", "message": title + " " + detail})
	}
	return true
}
