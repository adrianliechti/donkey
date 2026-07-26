package gateway

import (
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// handler is the gateway's request path: route lookup, session, access
// rules, then the proxy. The /oauth2/* paths are the gateway's own on every
// protected hostname — the price of putting login in front of an app.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth2/callback", s.handleCallback)
	mux.HandleFunc("GET /oauth2/logout", s.handleLogout)
	mux.HandleFunc("/", s.serveApp)
	return mux
}

func (s *Server) serveApp(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.lookup(r.Context(), requestHost(r), r.URL.Path)
	if !ok {
		s.errorPage(w, http.StatusNotFound, "Nothing is served here.",
			"If this app was just deployed, give it a moment and reload.")
		return
	}
	if !rt.Login {
		s.proxy(rt).ServeHTTP(w, r)
		return
	}
	// The route demands a login. If the installation cannot run one, the
	// promise of privacy wins over availability: fail closed.
	if !s.cfg.authConfigured() {
		s.errorPage(w, http.StatusServiceUnavailable, "This app requires a sign-in, but none is configured.",
			"The installation has no (complete) OIDC setup — `donkey server install` with auth.issuer, clientId and clientSecret fixes this.")
		return
	}
	sess := s.sessionFrom(r)
	if sess == nil {
		// Browsers get the login round trip; API calls get a plain 401 — a
		// redirect chain into an IdP is useless to a fetch() or a CLI.
		if wantsHTML(r) {
			s.startLogin(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="donkey"`)
		http.Error(w, `{"error":"sign-in required"}`, http.StatusUnauthorized)
		return
	}
	if !allowedAccess(rt.Access, sess) {
		who := sess.Email
		if who == "" {
			who = sess.Subject
		}
		s.deniedPage(w, who)
		return
	}
	s.proxy(rt).ServeHTTP(w, r)
}

// wantsHTML says whether the request came from a navigating browser.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// identityHeaders is what the app learns about its visitor. Incoming values
// are always dropped first — identity must be unforgeable.
var identityHeaders = []string{"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups", "X-Auth-Request-Roles"}

// transport is shared across requests: connection reuse is what keeps the
// extra hop cheap. No response-header timeout — apps stream.
var transport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
	ForceAttemptHTTP2:   false,
}

func (s *Server) proxy(rt route) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			sess := s.sessionFrom(pr.In)
			pr.SetURL(rt.Backend)
			// The app sees the address the visitor used — links, redirects
			// and host-based routing inside the app keep working.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			// This hop is plain http behind the TLS-terminating Gateway; the
			// app must still generate https URLs.
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			for _, h := range identityHeaders {
				pr.Out.Header.Del(h)
			}
			// The session cookie is the gateway's, not the app's — don't leak
			// sealed identity material into app code.
			stripCookie(pr.Out, sessionCookie, stateCookie)
			if sess != nil {
				pr.Out.Header.Set("X-Auth-Request-User", sess.Subject)
				if sess.Email != "" {
					pr.Out.Header.Set("X-Auth-Request-Email", sess.Email)
				}
				if len(sess.Groups) > 0 {
					pr.Out.Header.Set("X-Auth-Request-Groups", strings.Join(sess.Groups, ","))
				}
				if len(sess.Roles) > 0 {
					pr.Out.Header.Set("X-Auth-Request-Roles", strings.Join(sess.Roles, ","))
				}
			}
		},
		Transport: transport,
		// Flush as bytes arrive: SSE and long-running responses must not sit
		// in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.errorPage(w, http.StatusBadGateway, "The app didn't answer.",
				"It may be starting up or crashed — its logs know more. ("+err.Error()+")")
		},
	}
}

// stripCookie removes the named cookies from the outgoing request, keeping
// whatever cookies the app itself set.
func stripCookie(r *http.Request, names ...string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		drop := false
		for _, name := range names {
			if c.Name == name {
				drop = true
				break
			}
		}
		if !drop {
			r.AddCookie(c)
		}
	}
}
