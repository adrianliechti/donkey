package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Retain the public /ai/v1 forwarding route for existing clients. New consoles
// use the authenticated per-service route above. The platform's upstream key
// stays in donkey-ai, which has no control-plane write permissions.
//
// Nothing is verified here. donkey-ai checks the space's binding token, the
// same one an app sends; this hop only carries it. It is deliberately not
// behind s.auth, because the credential in play is the binding's, not an
// operator's.
const aiForwardPrefix = "/ai/v1/"

// aiForwardHandler proxies the console's AI calls to donkey-ai. The "/ai" is
// this origin's business — donkey-ai serves plain /v1, the same path apps
// use — so it is stripped before the request leaves.
func aiForwardHandler(target string) http.Handler {
	if target == "" {
		// Nothing to forward to. Saying so beats falling through to the
		// console's index.html, which is what an OpenAI client would then try
		// to parse as JSON.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable,
				fmt.Errorf("this installation has no AI endpoint configured (DONKEY_AI_URL)"))
		})
	}
	base, err := url.Parse(target)
	if err != nil || base.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable,
				fmt.Errorf("the AI endpoint address %q is not a URL", target))
		})
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL joins the base with the (already stripped) request path,
			// so donkey-ai sees exactly the /v1/... an app would send. It also
			// points the Host header at the target, which is what we want.
			pr.SetURL(base)
			// The console arrives on a route the gateway marks for token
			// forwarding, so its request carries the sealed admin credential.
			// donkey-ai authorizes on the space's binding token and has no use
			// for that one — it should not travel further than the API that
			// verifies it.
			pr.Out.Header.Del("X-Access-Token")
		},
		// Model responses stream as SSE; buffering them would turn a live
		// answer into one long silence.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the AI endpoint didn't answer: %w", err))
		},
	}
	return http.StripPrefix("/ai", proxy)
}
