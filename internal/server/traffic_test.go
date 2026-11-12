package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

func TestSpaceTrafficVisibleToMembers(t *testing.T) {
	s, rt := spaceTestServer()
	policy := rt.settings["team"]
	policy.Developers = config.AccessConfig{Users: []string{"dev@example.com"}}
	policy.Viewers = config.AccessConfig{Users: []string{"viewer@example.com"}}
	rt.settings["team"] = policy
	queries := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		if r.URL.Path != "/v1/traffic" || r.URL.Query().Get("space") != "team" || r.URL.Query().Get("app") != "" {
			t.Errorf("unexpected telemetry query: %s", r.URL)
		}
		writeJSON(w, http.StatusOK, telemetry.TrafficReport{Totals: telemetry.TrafficTotals{Requests: 42}})
	}))
	defer upstream.Close()
	s.telemetry = telemetry.NewClient(upstream.URL, nil)

	for _, tc := range []struct {
		email, space string
		status       int
	}{
		{"dev@example.com", "team", 200},
		{"viewer@example.com", "team", 200},
		{"owner@example.com", "team", 200},
		{"root@example.com", "team", 200},
		{"outsider@example.com", "team", 403},
		{"dev@example.com", "other", 403},
	} {
		t.Run(tc.email+"/"+tc.space, func(t *testing.T) {
			rec := callAs(t, s, tc.email, "GET", "/v1/spaces/"+tc.space+"/traffic", nil)
			if rec.Code != tc.status {
				t.Fatalf("traffic: %d %s", rec.Code, rec.Body)
			}
			if tc.status == 200 {
				var report telemetry.TrafficReport
				if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil || report.Totals.Requests != 42 {
					t.Fatalf("missing space traffic: %s, %v", rec.Body, err)
				}
			}
		})
	}
	if queries != 4 {
		t.Fatalf("unauthorized requests reached telemetry: %d queries", queries)
	}
}
