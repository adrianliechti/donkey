package build

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"monobox.ch/donkey/internal/config"
)

// CheckRegistryAuth probes the registry's /v2/ endpoint and verifies the
// configured credentials work. It supports Basic auth directly and the
// Bearer token flow (WWW-Authenticate: Bearer realm=...) that most
// registries use.
func CheckRegistryAuth(ctx context.Context, reg config.Registry) error {
	password, err := reg.Password()
	if err != nil {
		return err
	}
	scheme := "https"
	if reg.Insecure {
		scheme = "http"
	}
	base := scheme + "://" + reg.Host() + "/v2/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the registry %s didn't answer: %w", reg.Host(), err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil // anonymous access allowed
	case http.StatusUnauthorized:
	default:
		return fmt.Errorf("the registry %s answered unexpectedly (HTTP %d)", reg.Host(), resp.StatusCode)
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	if strings.HasPrefix(strings.ToLower(challenge), "basic") {
		return checkBasic(ctx, base, reg.Username, password)
	}
	if strings.HasPrefix(strings.ToLower(challenge), "bearer") {
		return checkBearer(ctx, challenge, reg.Username, password)
	}
	return fmt.Errorf("the registry %s uses an authentication scheme donkey doesn't recognize (%q)", reg.Host(), challenge)
}

func checkBasic(ctx context.Context, endpoint, user, password string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.SetBasicAuth(user, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("the registry rejected the username or password")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry answered unexpectedly (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// checkBearer fetches a token from the realm in the Bearer challenge; a 401
// from the realm means the credentials are wrong.
func checkBearer(ctx context.Context, challenge, user, password string) error {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("the registry sent an incomplete authentication challenge")
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if user != "" || password != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the registry rejected the username or password")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry's login service answered unexpectedly (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// parseChallenge extracts key="value" pairs from a WWW-Authenticate header.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	_, rest, _ := strings.Cut(h, " ")
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	return out
}
