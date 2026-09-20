package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
)

// The authenticated service console works for any OpenAI-compatible provider.
// The target and upstream credential come exclusively from its saved binding;
// no operator credential or browser cookie may cross this boundary.
func (s *Server) handleServiceAI(w http.ResponseWriter, r *http.Request) {
	service, err := s.serviceConnection(r.Context(), r, addons.ProtocolOpenAI)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	base, _ := url.Parse(service.Binding["OPENAI_BASE_URL"])
	path := "/models"
	if r.Method == http.MethodPost {
		path = "/chat/completions"
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.SetURL(base)
			// Forward only the headers an OpenAI client needs. In particular,
			// strip the incoming Authorization, Cookie and X-Access-Token.
			pr.Out.Header = make(http.Header)
			pr.Out.Header.Set("Content-Type", "application/json")
			pr.Out.Header.Set("Accept", pr.In.Header.Get("Accept"))
			if key := service.Binding["OPENAI_API_KEY"]; key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+key)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if r.Method != http.MethodPost || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				res.Header.Set("Content-Type", "application/json")
			}
			res.Header.Set("X-Content-Type-Options", "nosniff")
			// A redirect would send the browser outside the authenticated
			// service route; report it as an upstream failure instead.
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return fmt.Errorf("the service's AI endpoint redirected the request")
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Transport errors can contain URLs; keep binding values private.
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the service's AI endpoint did not answer"))
		},
	}
	// SetURL joins paths; one slash at the boundary is sufficient.
	base.Path = strings.TrimRight(base.Path, "/")
	proxy.ServeHTTP(w, r)
}

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
