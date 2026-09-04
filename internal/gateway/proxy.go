package gateway

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"

	"github.com/adrianliechti/donkey/internal/identity"
)

// handler is the gateway's request path: route lookup, session, access
// rules, then the proxy. The /oauth2/* paths are the gateway's own on every
// protected hostname — the price of putting login in front of an app.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth2/callback", s.handleCallback)
	mux.HandleFunc("GET /oauth2/cli", s.startCLILogin)
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
	// Who this is, decided once: the session cookie (a browser) or a sealed
	// CLI token as Bearer. Public routes carry the identity too — an app may
	// greet a signed-in visitor without demanding the login.
	sess, fromToken := s.identityFrom(r)
	if rt.Login {
		// The route demands a login. If the installation cannot run one, the
		// promise of privacy wins over availability: fail closed.
		if !s.cfg.authConfigured() {
			s.errorPage(w, http.StatusServiceUnavailable, "This app requires a sign-in, but none is configured.",
				"The installation has no (complete) OIDC setup — `donkey server install` with auth.issuer, clientId and clientSecret fixes this.")
			return
		}
		if sess == nil {
			// Browsers get the login round trip; API calls get a plain 401 —
			// a redirect chain into an IdP is useless to a fetch() or a CLI.
			if wantsHTML(r) {
				s.startLogin(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="donkey"`)
			http.Error(w, `{"error":"sign-in required"}`, http.StatusUnauthorized)
			return
		}
		if !rt.Access.Allows(sess.Email, sess.Groups, sess.Roles) {
			who := sess.Email
			if who == "" {
				who = sess.Subject
			}
			s.deniedPage(w, who)
			return
		}
	}
	metrics := httpsnoop.CaptureMetrics(s.proxy(rt, sess, fromToken), w, r)
	if s.telemetry != nil {
		s.telemetry.Record(r, rt, sess, metrics.Code, metrics.Duration, w.Header().Get("Content-Type"))
	}
}

// wantsHTML says whether the request came from a navigating browser.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// identityHeaders is what the app learns about its visitor. Incoming values
// are always dropped first — identity must be unforgeable.
var identityHeaders = []string{"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups", "X-Auth-Request-Roles"}

// accessTokenHeader carries a sealed credential to the one backend that
// verifies identity itself (donkey's own API). Inbound copies are dropped on
// every route, exactly like the identity headers.
const accessTokenHeader = "X-Access-Token"

// forwardTokenTTL bounds the token the gateway mints per request. Short: it
// exists for the hop to the backend, not for keeping — a leaked one expires
// before it is worth stealing. Never longer than the session it came from.
const forwardTokenTTL = 10 * time.Minute

// transport is shared across requests: connection reuse is what keeps the
// extra hop cheap. No response-header timeout — apps stream.
var transport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
	ForceAttemptHTTP2:   false,
}

// proxy forwards to the app. A ReverseProxy is built per request, so the
// identity serveApp established rides in the closure — nothing is verified
// twice.
func (s *Server) proxy(rt route, sess *session, fromToken bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			if fromToken {
				// The token authenticated to the gateway; the app gets the
				// identity headers, never the credential itself.
				pr.Out.Header.Del("Authorization")
			}
			pr.SetURL(rt.Backend)
			pr.Out.Header.Del("X-Forwarded-Prefix")
			if rt.StripPrefix {
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, rt.Path)
				if pr.Out.URL.Path == "" {
					pr.Out.URL.Path = "/"
				}
				// Count decoded bytes so an escaped prefix does not discard
				// meaningful escapes (such as %2F) in the remaining API path.
				escaped, cut := pr.In.URL.EscapedPath(), 0
				for n := 0; n < len(rt.Path) && cut < len(escaped); n++ {
					if escaped[cut] == '%' {
						cut += 3
					} else {
						cut++
					}
				}
				pr.Out.URL.RawPath = escaped[cut:]
				if pr.Out.URL.RawPath == "" {
					pr.Out.URL.RawPath = "/"
				}
				pr.Out.Header.Set("X-Forwarded-Prefix", rt.Path)
			}
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
			pr.Out.Header.Del(accessTokenHeader)
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
				// The API route gets a verifiable credential, not bare
				// headers: the server checks the seal, so an in-cluster
				// caller typing headers gets nothing. Only this route — an
				// app handed a token could replay its visitors against the
				// API.
				if rt.Token {
					exp := time.Now().Add(forwardTokenTTL).Unix()
					if sess.Expires > 0 && sess.Expires < exp {
						exp = sess.Expires
					}
					tok, err := identity.SealToken(s.cfg.SessionKey, identity.Identity{
						Subject: sess.Subject, Email: sess.Email, Name: sess.Name,
						Groups: sess.Groups, Roles: sess.Roles, Expires: exp,
					})
					if err == nil {
						pr.Out.Header.Set(accessTokenHeader, tok)
					}
				}
			}
		},
		ModifyResponse: func(res *http.Response) error {
			// An API mounted under a proxy prefix may redirect to /login or
			// add a trailing slash. Keep that redirect on the API's route.
			location := res.Header.Get("Location")
			u, err := url.Parse(location)
			if !rt.StripPrefix || err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(location, "/") {
				return nil
			}
			if u.Path != rt.Path && !strings.HasPrefix(u.Path, rt.Path+"/") {
				res.Header.Set("Location", rt.Path+location)
			}
			return nil
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
