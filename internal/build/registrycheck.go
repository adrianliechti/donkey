package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"

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
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: http.ProxyFromEnvironment}}, nil
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

// DefaultScannerDBRepository is where the scanner fetches its vulnerability
// database when the installation names no mirror — the scanner's own
// default, checked by doctor so a restricted network learns early that it
// cannot reach it. The scanner falls back to ghcr.io/aquasecurity/trivy-db
// on its own.
const DefaultScannerDBRepository = "mirror.gcr.io/aquasec/trivy-db"

// Tags the scanner's databases are published under: a schema version, not
// a release.
var scannerDBTags = map[string]string{
	"trivy-db":      "2",
	"trivy-java-db": "1",
}

// CheckOCIArtifact asks the repository for the artifact's manifest the way
// the scanner will: anonymously, or with donkey's credentials for that host
// after a Basic or Bearer challenge. It never sends a credential over plain
// HTTP.
func CheckOCIArtifact(ctx context.Context, reg config.Registry, repo string) error {
	named, err := reference.ParseNormalizedNamed(repo)
	if err != nil {
		return fmt.Errorf("%q is not a repository reference: %w", repo, err)
	}
	host, path := reference.Domain(named), reference.Path(named)
	tag := ""
	if tagged, ok := named.(reference.Tagged); ok {
		tag = tagged.Tag()
	}
	if tag == "" {
		tag = "2"
		for suffix, t := range scannerDBTags {
			if strings.HasSuffix(path, "/"+suffix) || path == suffix {
				tag = t
			}
		}
	}
	endpoint := host
	if host == "docker.io" {
		endpoint = "registry-1.docker.io"
	}
	scheme := "https"
	if reg.Insecure && sameRegistryHost(host, reg.Host()) {
		scheme = "http"
	}
	client, err := registryClient(reg)
	if err != nil {
		return err
	}
	auth, err := registryAuthConfig(reg, host)
	if err != nil {
		return err
	}
	manifest := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, endpoint, path, tag)
	head := func(authorization string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifest, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
		}, ", "))
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		return client.Do(req)
	}
	resp, err := head("")
	if err != nil {
		return fmt.Errorf("did not answer: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		authorization := ""
		switch {
		case strings.HasPrefix(strings.ToLower(challenge), "basic"):
			if scheme != "https" {
				return fmt.Errorf("asks for a login over plain HTTP, which donkey won't answer")
			}
			authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(auth.Username+":"+auth.Password))
		case strings.HasPrefix(strings.ToLower(challenge), "bearer"):
			token, err := bearerToken(ctx, client, challenge, auth.Username, auth.Password, scheme == "https")
			if err != nil {
				return err
			}
			authorization = "Bearer " + token
		default:
			return fmt.Errorf("uses an authentication scheme donkey doesn't recognize (%q)", challenge)
		}
		if resp, err = head(authorization); err != nil {
			return fmt.Errorf("did not answer: %w", err)
		}
		resp.Body.Close()
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("has no %s tag (HTTP 404)", tag)
	case http.StatusUnauthorized, http.StatusForbidden:
		if auth.Username == "" {
			return fmt.Errorf("requires a login donkey has no credentials for (HTTP %d) — name the host under registry.dockerConfigDir", resp.StatusCode)
		}
		return fmt.Errorf("rejected donkey's credentials (HTTP %d)", resp.StatusCode)
	}
	return fmt.Errorf("answered unexpectedly (HTTP %d)", resp.StatusCode)
}

// bearerToken follows a Bearer challenge to its realm, with the credentials
// when donkey has some; an anonymous token is what public registries hand
// out for public artifacts.
func bearerToken(ctx context.Context, client *http.Client, challenge, username, password string, secure bool) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	realmURL, err := url.Parse(realm)
	if err != nil || realmURL.Host == "" {
		return "", fmt.Errorf("pointed donkey at %q for its login, which is not a usable address", realm)
	}
	credentials := username != "" && password != ""
	if credentials && (realmURL.Scheme != "https" || !secure) {
		return "", fmt.Errorf("wants donkey to log in at %s — donkey only sends credentials over https", realm)
	}
	q := url.Values{}
	for _, key := range []string{"service", "scope"} {
		if v := params[key]; v != "" {
			q.Set(key, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if credentials {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login service did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login service answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("login service answered with something other than a token: %w", err)
	}
	if body.Token == "" {
		body.Token = body.AccessToken
	}
	if body.Token == "" {
		return "", fmt.Errorf("login service handed out no token")
	}
	return body.Token, nil
}

// CheckImageResolves asks the build engine to resolve an image the way a
// build would — through its configured mirrors, with donkey's session
// answering token requests — without pulling it.
func CheckImageResolves(ctx context.Context, addr string, reg config.Registry, mirrors map[string]string, image, platform string) error {
	if addr == "" {
		return fmt.Errorf("no build engine address configured")
	}
	// The gateway's resolver takes the reference as given: a Docker Hub
	// short name has to be spelled out the way llb.Image would.
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("%q is not an image reference: %w", image, err)
	}
	image = reference.TagNameOnly(named).String()
	c, err := bkclient.New(ctx, addr)
	if err != nil {
		return fmt.Errorf("connecting to the build engine: %w", err)
	}
	defer c.Close()
	auth, err := registryAuth(reg, mirrors)
	if err != nil {
		return err
	}
	var p *ocispecs.Platform
	if platform != "" {
		parsed, err := platforms.Parse(platform)
		if err != nil {
			return fmt.Errorf("platform %q: %w", platform, err)
		}
		p = &parsed
	}
	// The status channel must be drained or the solve never finishes.
	ch := make(chan *bkclient.SolveStatus)
	go func() {
		for range ch {
		}
	}()
	_, err = c.Build(ctx, bkclient.SolveOpt{Session: []session.Attachable{auth}}, "donkey-doctor", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		_, _, _, err := gw.ResolveImageConfig(ctx, image, sourceresolver.Opt{ImageOpt: &sourceresolver.ResolveImageOpt{Platform: p, NoConfig: true}})
		return gateway.NewResult(), err
	}, ch)
	return err
}
