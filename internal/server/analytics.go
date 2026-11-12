package server

import (
	"net/http"

	"github.com/adrianliechti/donkey/internal/telemetry"
)

// handleAnalytics is an app's web analytics report: what the gateway's
// script counted on its pages, sliced by the dimensions in ?filter=.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	selected, err := telemetry.ParseTrafficRange(r.URL.Query().Get("range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	filters, err := telemetry.ParseAnalyticsFilters(r.URL.Query()["filter"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.telemetry.Analytics(r.Context(), space, app, selected, filters)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleAnalyticsLive(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	live, err := s.telemetry.AnalyticsLive(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, live)
}
