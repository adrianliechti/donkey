package server

// The AI proxy behind the openai addon. Apps call an OpenAI-compatible API
// at donkey-server's own address with a per-space token; the proxy checks
// the token against the bindings it issued and swaps in the platform's real
// credential on the way upstream. The real key never leaves donkey-system,
// and removing the service revokes the space's access.

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
)

const aiProxyPrefix = "/ai/v1/"

// aiAllowedRoots is the OpenAI-compatible inference surface this addon
// offers. The credential swapped in upstream is the platform's own, so the
// proxy hands out inference and nothing else: files, fine_tuning, assistants,
// batches, uploads and vector_stores would let any space's binding delete or
// rewrite state belonging to whoever owns the account.
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

// aiUpstreamPath turns a request path into the path to join onto the upstream
// base, or reports that it may not be proxied.
//
// The containment check is the load-bearing part. ServeMux cleans the
// *escaped* path, so "%2e%2e" and "..%2f" survive routing and only become
// ".." once URL.Path is decoded here — at which point url.JoinPath's
// path.Join would happily resolve them out of the base ("/ai/v1/%2e%2e/%2e%2e/
// admin/keys" reaching the upstream as "/admin/keys"), attaching the
// platform's key to any path on the upstream host. fs.ValidPath is the
// standard library's definition of a clean relative path: no "..", no leading
// slash, no empty elements.
func aiUpstreamPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, aiProxyPrefix)
	if !fs.ValidPath(strings.TrimSuffix(rest, "/")) {
		return "", false
	}
	root, _, _ := strings.Cut(rest, "/")
	if !aiAllowedRoots[root] {
		return "", false
	}
	return rest, true
}

// aiPathKey carries the validated path to the Rewrite hook, so the path that
// is proxied is exactly the one that passed the checks above.
type aiPathKey struct{}

// aiTokens caches the valid per-space tokens so the hot path doesn't hit
// the Kubernetes API per request. A short TTL keeps revocation honest.
type aiTokens struct {
	mu      sync.Mutex
	until   time.Time
	tokens  map[string]bool
	refresh func() (map[string]bool, error)
}

func (t *aiTokens) valid(token string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().After(t.until) || t.tokens == nil {
		fresh, err := t.refresh()
		if err != nil {
			return false, err
		}
		t.tokens = fresh
		t.until = time.Now().Add(30 * time.Second)
	}
	return t.tokens[token], nil
}

func (s *Server) aiProxyHandler() http.Handler {
	upstreamBase := strings.TrimRight(os.Getenv("OPENAI_BASE_URL"), "/")
	if upstreamBase == "" {
		upstreamBase = "https://api.openai.com/v1"
	}
	upstreamKey := os.Getenv("OPENAI_API_KEY")
	upstream, err := url.Parse(upstreamBase)
	if err != nil {
		// The assistant already validated this config at startup; a broken
		// URL here is unreachable in practice.
		panic(fmt.Sprintf("OPENAI_BASE_URL is not a URL: %v", err))
	}

	tokens := &aiTokens{refresh: func() (map[string]bool, error) {
		// The tokens are whatever OPENAI_API_KEY values the space bindings
		// hold — the binding Secret is the single source of truth, so
		// removing the service is the revocation.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		values, err := s.k8s.SharedBindingValues(ctx, addons.SharedOwner, "OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, v := range values {
			set[v] = true
		}
		return set, nil
	}}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest, _ := pr.In.Context().Value(aiPathKey{}).(string)
			pr.Out.URL = upstream.JoinPath(rest)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = upstream.Host
			// The space's stand-in token goes no further than this hop.
			pr.Out.Header.Set("Authorization", "Bearer "+upstreamKey)
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
			writeErr(w, http.StatusBadGateway, err)
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
		if !ok {
			writeErr(w, http.StatusForbidden, fmt.Errorf("the AI service proxies inference only (chat, completions, embeddings, models, responses, moderations, audio, images)"))
			return
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), aiPathKey{}, rest)))
	})
}
