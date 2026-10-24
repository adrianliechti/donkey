package aiproxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

const ExaPrefix = "/exa/"

// ExaHandler fronts only search and contents; account-management APIs never
// receive the platform credential. Custom bases may include a gateway prefix.
func ExaHandler(upstreamBase, upstreamKey string, valid Tokens) (http.Handler, error) {
	if upstreamBase == "" {
		upstreamBase = config.DefaultExaBaseURL
	}
	if err := (config.InstallExa{BaseURL: upstreamBase}).Validate(); err != nil {
		return nil, err
	}
	upstream, _ := url.Parse(strings.TrimRight(upstreamBase, "/"))
	// JoinPath on an empty path yields a relative one ("search"), which the
	// proxy would send as is in the request line — the default base has none.
	if upstream.Path == "" {
		upstream.Path = "/"
	}
	tokens := newTokenCache(valid)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			path := strings.TrimPrefix(pr.In.URL.Path, ExaPrefix)
			pr.Out.URL = upstream.JoinPath(path)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = upstream.Host
			pr.Out.Header = make(http.Header)
			pr.Out.Header.Set("Content-Type", "application/json")
			pr.Out.Header.Set("Accept", pr.In.Header.Get("Accept"))
			pr.Out.Header.Set("x-api-key", upstreamKey)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return fmt.Errorf("the search endpoint redirected the request")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the search endpoint did not answer"))
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamKey == "" {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("the platform's Exa service is not configured (EXA_API_KEY)"))
			return
		}
		token := r.Header.Get("x-api-key")
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == r.Header.Get("Authorization") {
				token = ""
			}
		}
		if token == "" {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("send the binding's EXA_API_KEY in x-api-key or as a bearer token"))
			return
		}
		ok, err := tokens.valid(token)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("could not verify the Exa binding"))
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("connect the exa service and use its binding key"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("the Exa service takes POST"))
			return
		}
		if r.URL.Path != ExaPrefix+"search" && r.URL.Path != ExaPrefix+"contents" {
			writeErr(w, http.StatusForbidden, fmt.Errorf("the Exa service proxies search and contents only"))
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
