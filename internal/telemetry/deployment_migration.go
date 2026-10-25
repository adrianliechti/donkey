package telemetry

// Temporary compatibility for the orchestrator's one-off deployment-data
// migration. Remove with the next release, along with the legacy build API.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type BuildScope struct {
	Space string `json:"space"`
	App   string `json:"app"`
}

func (s *Service) handleBuildScopes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.db.QueryContext(r.Context(), "SELECT DISTINCT space, app FROM build_records ORDER BY space, app")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	scopes := []BuildScope{}
	for rows.Next() {
		var scope BuildScope
		if err := rows.Scan(&scope.Space, &scope.App); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeServiceJSON(w, http.StatusOK, scopes)
}

func (c *Client) BuildScopes(ctx context.Context) ([]BuildScope, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("telemetry service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/builds/scopes", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading legacy deployment scopes: telemetry returned %s", response.Status)
	}
	var scopes []BuildScope
	if err := json.NewDecoder(response.Body).Decode(&scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}
