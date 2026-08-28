package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
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
		http:    &http.Client{Timeout: 10 * time.Second},
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
