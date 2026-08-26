package build

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

// CheckRegistryAuth probes the registry's /v2/ endpoint and verifies the
// configured credentials work. It supports Basic auth directly and the
// Bearer token flow (WWW-Authenticate: Bearer realm=...) that most
// registries use.
func CheckRegistryAuth(ctx context.Context, reg config.Registry) error {
	username, password, err := reg.Credentials()
	if err != nil {
		return err
	}
	scheme := "https"
	if reg.Insecure {
		scheme = "http"
	}
	client, err := registryClient(reg)
	if err != nil {
		return err
	}
	base := scheme + "://" + reg.Host() + "/v2/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
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
		return checkBasic(ctx, client, base, username, password)
	}
	if strings.HasPrefix(strings.ToLower(challenge), "bearer") {
		return checkBearer(ctx, client, reg, challenge, username, password)
	}
	return fmt.Errorf("the registry %s uses an authentication scheme donkey doesn't recognize (%q)", reg.Host(), challenge)
}

// registryClient trusts the registry's own certificate authority when the
// installation named one.
func registryClient(reg config.Registry) (*http.Client, error) {
	tlsConfig, err := reg.TLSConfig()
	if err != nil {
		return nil, err
	}
	if tlsConfig == nil {
		return http.DefaultClient, nil
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

func checkBasic(ctx context.Context, client *http.Client, endpoint, user, password string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.SetBasicAuth(user, password)
	resp, err := client.Do(req)
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
//
// The realm is an address the registry picks, and donkey answers it with the
// registry's password — so it is only followed when the answer cannot be
// read on the way: https, and never from a registry donkey talks to in plain
// HTTP (where anyone on the path could have written the challenge).
func checkBearer(ctx context.Context, client *http.Client, reg config.Registry, challenge, username, password string) error {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("the registry sent an incomplete authentication challenge")
	}
	credentials := username != "" || password != ""
	realmURL, err := url.Parse(realm)
	if err != nil || realmURL.Host == "" {
		return fmt.Errorf("the registry %s pointed donkey at %q for its login, which is not a usable address", reg.Host(), realm)
	}
	if realmURL.Scheme != "https" {
		return fmt.Errorf("the registry %s wants donkey to log in at %s — donkey only sends registry credentials over https", reg.Host(), realm)
	}
	if reg.Insecure && credentials {
		return fmt.Errorf("the registry %s is configured as insecure (plain HTTP) but asks for a login at %s — donkey won't send the password, because the request that asked for it wasn't protected", reg.Host(), realm)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if credentials {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
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
