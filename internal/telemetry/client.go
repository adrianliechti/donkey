package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL string, key []byte) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   QueryToken(key),
		http:    &http.Client{},
	}
}

func (c *Client) Traffic(ctx context.Context, space, app, source string, selected TrafficRange) (TrafficReport, error) {
	if c == nil || c.baseURL == "" {
		return TrafficReport{}, errors.New("telemetry service is not configured")
	}
	query := url.Values{"space": {space}, "range": {string(selected)}}
	if app != "" {
		query.Set("app", app)
	}
	if source != "" {
		query.Set("source", source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/traffic?"+query.Encode(), nil)
	if err != nil {
		return TrafficReport{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return TrafficReport{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return TrafficReport{}, fmt.Errorf("telemetry service returned %s", response.Status)
	}
	var report TrafficReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return TrafficReport{}, err
	}
	return report, nil
}

func (c *Client) Resources(ctx context.Context, space, app string, selected ResourceRange) (ResourceReport, error) {
	if c == nil || c.baseURL == "" {
		return ResourceReport{}, errors.New("telemetry service is not configured")
	}
	query := url.Values{
		"space": {space},
		"app":   {app},
		"range": {string(selected)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/resources?"+query.Encode(), nil)
	if err != nil {
		return ResourceReport{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return ResourceReport{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ResourceReport{}, fmt.Errorf("telemetry service returned %s", response.Status)
	}
	var report ResourceReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return ResourceReport{}, err
	}
	return report, nil
}

// Analytics reads one app's web analytics report.
func (c *Client) Analytics(ctx context.Context, space, app string, selected TrafficRange, filters []AnalyticsFilter) (AnalyticsReport, error) {
	query := url.Values{"space": {space}, "app": {app}, "range": {string(selected)}}
	for _, filter := range filters {
		query.Add("filter", string(filter.Dimension)+":"+filter.Value)
	}
	var report AnalyticsReport
	return report, c.getJSON(ctx, "/v1/analytics?"+query.Encode(), &report)
}

// AnalyticsLive reads who is on one app right now.
func (c *Client) AnalyticsLive(ctx context.Context, space, app string) (AnalyticsLive, error) {
	var live AnalyticsLive
	return live, c.getJSON(ctx, "/v1/analytics/live?"+url.Values{"space": {space}, "app": {app}}.Encode(), &live)
}

func (c *Client) getJSON(ctx context.Context, path string, target any) error {
	if c == nil || c.baseURL == "" {
		return errors.New("telemetry service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// The service's 400 names the bad range or filter; pass it on.
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		if text := strings.TrimSpace(string(message)); text != "" && response.StatusCode == http.StatusBadRequest {
			return fmt.Errorf("telemetry service: %s", text)
		}
		return fmt.Errorf("telemetry service returned %s", response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (c *Client) PurgeSpace(ctx context.Context, space string) error {
	return c.delete(ctx, "/v1/spaces/"+url.PathEscape(space))
}

// PurgeBuilds removes an app's build history.
func (c *Client) PurgeBuilds(ctx context.Context, space, app string) error {
	return c.delete(ctx, "/v1/builds?"+url.Values{"space": {space}, "app": {app}}.Encode())
}

func (c *Client) delete(ctx context.Context, path string) error {
	if c == nil || c.baseURL == "" {
		return errors.New("telemetry service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry service returned %s", response.Status)
	}
	return nil
}

func (c *Client) Ready(ctx context.Context) error {
	if c == nil || c.baseURL == "" {
		return errors.New("telemetry service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry service returned %s", response.Status)
	}
	return nil
}

// RecordBuild appends one build record to an app's history and returns its
// sequence number.
func (c *Client) RecordBuild(ctx context.Context, space, app, image string, ok bool, record json.RawMessage) (int64, error) {
	if c == nil || c.baseURL == "" {
		return 0, errors.New("telemetry service is not configured")
	}
	body, err := json.Marshal(map[string]any{
		"space": space, "app": app, "image": image, "ok": ok, "record": record,
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/v1/builds", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telemetry service returned %s", response.Status)
	}
	var stored struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.NewDecoder(response.Body).Decode(&stored); err != nil {
		return 0, err
	}
	return stored.Sequence, nil
}

// Builds lists an app's retained build history, newest first.
func (c *Client) Builds(ctx context.Context, space, app string) ([]BuildSummary, error) {
	if c == nil || c.baseURL == "" {
		return nil, errors.New("telemetry service is not configured")
	}
	query := url.Values{"space": {space}, "app": {app}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/builds?"+query.Encode(), nil)
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
		return nil, fmt.Errorf("telemetry service returned %s", response.Status)
	}
	var listed struct {
		Builds []BuildSummary `json:"builds"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		return nil, err
	}
	return listed.Builds, nil
}

// Build fetches one stored record by sequence; BuildByImage fetches the
// newest record for an exact image reference. Both return ErrNoBuild when
// nothing matches.
func (c *Client) Build(ctx context.Context, space, app string, sequence int64) (json.RawMessage, error) {
	return c.buildRecord(ctx, url.Values{
		"space": {space}, "app": {app}, "sequence": {strconv.FormatInt(sequence, 10)},
	})
}

func (c *Client) BuildByImage(ctx context.Context, space, app, image string) (json.RawMessage, error) {
	return c.buildRecord(ctx, url.Values{"space": {space}, "app": {app}, "image": {image}})
}

func (c *Client) buildRecord(ctx context.Context, query url.Values) (json.RawMessage, error) {
	if c == nil || c.baseURL == "" {
		return nil, errors.New("telemetry service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/builds/record?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNoBuild
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telemetry service returned %s", response.Status)
	}
	record, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return record, nil
}
