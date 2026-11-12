package gateway

import (
	"fmt"
	"net/http"

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
