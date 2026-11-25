// Package aiproxy authenticates model requests from development machines.
package aiproxy

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/identity"
)

// Prefix is where the proxy is mounted. donkey-ai serves it at the root of
// its own listener, so a machine's OPENAI_BASE_URL is an ordinary /v1 endpoint.
const Prefix = "/v1/"

// staysClean checks each decoding of a relative path because an upstream
// gateway may decode it again before routing. The request must stay under
// the configured base path even when the provider sits behind a gateway.
func staysClean(p string) bool {
	for {
		// fs.ValidPath takes "..;" and backslashes as plain names, but a
		// gateway that reads ";" as a path parameter (Tomcat, Jetty) or "\"
		// as "/" turns them into a way up and out.
		if !fs.ValidPath(strings.TrimSuffix(p, "/")) || strings.ContainsAny(p, `;\`) {
			return false
		}
		// A malformed escape is refused rather than guessed at — what it
		// means is up to whoever decodes it next.
		decoded, err := url.PathUnescape(p)
		if err != nil {
			return false
		}
		if decoded == p {
			return true
		}
		p = decoded
	}
}

// Handler is the proxy. upstream/key are the platform's own endpoint and
// credential. Only running machines have valid model tokens.
func Handler(upstreamBase, upstreamKey string, valid Tokens) (http.Handler, error) {
	upstreamBase = strings.TrimRight(upstreamBase, "/")
	if upstreamBase == "" {
		upstreamBase = "https://api.openai.com/v1"
	}
	upstream, err := url.Parse(upstreamBase)
	if err != nil || addons.ValidateBinding(addons.ProtocolOpenAI, map[string]string{"OPENAI_BASE_URL": upstreamBase}) != nil {
		return nil, fmt.Errorf("the AI endpoint must be an http or https URL without credentials, query or fragment")
	}
	tokens := newTokenCache(valid)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// The machine's token goes no further than this hop.
			identity.StripHeaders(pr.Out.Header)
			for _, header := range []string{"Cookie", "X-Access-Token", "X-Api-Key", "Api-Key", "OpenAI-Organization", "OpenAI-Project"} {
				pr.Out.Header.Del(header)
			}
			pr.Out.Header.Set("Authorization", "Bearer "+upstreamKey)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return fmt.Errorf("the AI endpoint redirected the request")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the AI endpoint did not answer"))
		},
		// Token streams arrive as SSE — buffering them would turn streaming
		// into one long silence followed by everything at once.
		FlushInterval: -1,
	}

	forward := http.StripPrefix(strings.TrimSuffix(Prefix, "/"), proxy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("this endpoint speaks OpenAI — send the machine's OPENAI_API_KEY as a bearer token"))
			return
		}
		ok, err := tokens.valid(token)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("could not verify the machine token"))
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("the model token is not valid for a running development machine"))
			return
		}
		rest, ok := strings.CutPrefix(r.URL.EscapedPath(), Prefix)
		if !ok || (rest != "" && !staysClean(rest)) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid path under %s", Prefix))
			return
		}
		forward.ServeHTTP(w, r)
	}), nil
}

// writeErr answers in the shape an OpenAI client expects to parse.
func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
