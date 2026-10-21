// Package aiproxy serves the openai and exa addons: apps call it with a
// per-space token, which it checks against the bindings donkey issued and
// swaps for the platform's real credential. The key never leaves
// donkey-system, and removing the service revokes the space's access.
//
// It runs as its own workload (donkey-ai) rather than inside donkey-server,
// because this is the one donkey endpoint tenant code is *meant* to reach.
// Keeping it out of the admin API's process means a flaw in the part apps can
// talk to does not sit in the process that can write to the cluster.
package aiproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/adrianliechti/donkey/internal/addons"
	"strings"
)

// Prefix is where the proxy is mounted. donkey-ai serves it at the root of
// its own listener, so an app's OPENAI_BASE_URL is an ordinary /v1 endpoint.
const Prefix = "/v1/"

// Inference and nothing else: the credential swapped in is the platform's
// own, so files, fine_tuning, assistants, batches and vector_stores would let
// any space's binding rewrite state belonging to the account owner.
var aiAllowedRoots = map[string]bool{
	"chat":        true,
	"completions": true,
	"embeddings":  true,
	"models":      true,
	"responses":   true,
	"moderations": true,
	"audio":       true,
	"images":      true,
}

// aiUpstreamPath yields the path to join onto the upstream base, or reports
// that it may not be proxied.
func aiUpstreamPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, Prefix)
	if !staysClean(rest) {
		return "", false
	}
	root, _, _ := strings.Cut(rest, "/")
	if !aiAllowedRoots[root] {
		return "", false
	}
	return rest, true
}

// A shared upstream key must not expose stored conversations or response IDs
// belonging to another space. Only model discovery and fresh inference pass.
func aiAllowedRequest(method, path string) bool {
	if method == http.MethodGet {
		return path == "models" || strings.HasPrefix(path, "models/")
	}
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "chat/completions", "completions", "embeddings", "responses", "responses/input_tokens", "moderations",
		"audio/speech", "audio/transcriptions", "audio/translations", "images/generations", "images/edits", "images/variations":
		return true
	}
	return false
}

// staysClean reports whether p is a clean relative path and remains one
// however many times it is decoded. It is what keeps the proxied path inside
// the upstream's base, which is load-bearing here: the platform's own key is
// attached to whatever goes out.
//
// ServeMux cleans the *escaped* path, so "%2e%2e" survives routing and only
// becomes ".." once decoded — at which point url.JoinPath would resolve it out
// of the base. And one decoding is not enough: what goes on the wire is the
// escaped path, and each hop may decode again, so "%252e%252e" arrives here as
// the harmless-looking "%2e%2e", is forwarded as written, and becomes ".." at
// an upstream (or a gateway in front of one) that normalizes before it
// dispatches.
//
// Each round strictly shortens the string, so this ends: at the form that
// decodes into itself. fs.ValidPath is the standard library's definition of a
// clean relative path.
func staysClean(p string) bool {
	for {
		if !fs.ValidPath(strings.TrimSuffix(p, "/")) {
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

// aiPathKey carries the validated path to the Rewrite hook, so the path that
// is proxied is exactly the one that passed the checks above.
type aiPathKey struct{}

// Handler is the proxy. upstream/key are the platform's own endpoint and
// credential; valid answers which per-space tokens the bindings currently
// hold.
func Handler(upstreamBase, upstreamKey string, valid Tokens) (http.Handler, error) {
	upstreamBase = strings.TrimRight(upstreamBase, "/")
	if upstreamBase == "" {
		upstreamBase = "https://api.openai.com/v1"
	}
	upstream, err := url.Parse(upstreamBase)
	if err != nil || addons.ValidateBinding(addons.ProtocolOpenAI, map[string]string{"OPENAI_BASE_URL": upstreamBase}) != nil {
		return nil, fmt.Errorf("the AI endpoint must be an http or https URL without credentials, query or fragment")
	}
	// JoinPath on an empty path yields a relative one ("models"), which the
	// proxy would send as is in the request line.
	if upstream.Path == "" {
		upstream.Path = "/"
	}

	tokens := newTokenCache(valid)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest, _ := pr.In.Context().Value(aiPathKey{}).(string)
			pr.Out.URL = upstream.JoinPath(rest)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = upstream.Host
			// The space's stand-in token goes no further than this hop.
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("this endpoint speaks OpenAI — send the binding's OPENAI_API_KEY as a bearer token"))
			return
		}
		ok, err := tokens.valid(token)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("could not verify the AI binding"))
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("this key isn't one donkey issued — connect the openai service and use its binding"))
			return
		}
		// Inference is read-and-generate: anything else would be reaching for
		// the account's own state, not the model.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("the AI service takes GET and POST"))
			return
		}
		rest, ok := aiUpstreamPath(r.URL.Path)
		if !ok || !aiAllowedRequest(r.Method, rest) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("the AI service proxies inference only (chat, completions, embeddings, models, responses, moderations, audio, images)"))
			return
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), aiPathKey{}, rest)))
	}), nil
}

// writeErr answers in the shape an OpenAI client expects to parse.
func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
