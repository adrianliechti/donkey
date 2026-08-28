package server

import (
	"net/http"

	"github.com/adrianliechti/donkey/internal/telemetry"
)

func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	space, app := "", r.PathValue("app")
	if app == "" {
		var err error
		space, err = s.space(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	} else {
		var err error
		space, app, err = s.spaceApp(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	selected, err := telemetry.ParseTrafficRange(r.URL.Query().Get("range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.traffic.Traffic(r.Context(), space, app, r.URL.Query().Get("source"), selected)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
