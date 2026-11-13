package gateway

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"

	"github.com/adrianliechti/donkey/internal/analytics"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// handler is the gateway's request path: route lookup, session, access
// rules, then the proxy. The /oauth2/* paths are the gateway's own on every
// protected hostname — the price of putting login in front of an app.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCDiscovery)
	mux.HandleFunc("OPTIONS /.well-known/openid-configuration", s.handleOAuthPreflight)
	mux.HandleFunc("GET /oauth2/jwks", s.handleOIDCJWKS)
	mux.HandleFunc("GET /oauth2/userinfo", s.handleOIDCUserInfo)
	mux.HandleFunc("POST /oauth2/userinfo", s.handleOIDCUserInfo)
	mux.HandleFunc("POST /oauth2/revoke", s.handleOIDCRevoke)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleOAuthResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/", s.handleOAuthResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleOAuthAuthorizationServerMetadata)
	mux.HandleFunc("OPTIONS /.well-known/oauth-authorization-server", s.handleOAuthPreflight)
	mux.HandleFunc("POST /oauth2/register", s.handleOAuthRegister)
	mux.HandleFunc("GET /oauth2/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth2/authorize", s.handleOAuthAuthorizeConsent)
	mux.HandleFunc("POST /oauth2/token", s.handleOAuthToken)
	mux.HandleFunc("OPTIONS /oauth2/{path...}", s.handleOAuthPreflight)
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
	if strings.HasPrefix(r.URL.Path, analytics.Prefix) && s.serveAnalytics(w, r) {
		return
	}
	r = s.observeTraffic(r, rt)
	var sess *session
	metrics := httpsnoop.CaptureMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess = s.serveRoute(w, r, rt)
	}), w, r)
	for _, observation := range trafficObservations(r) {
		if observation.finish != nil {
			observation.finish(sess, metrics.Code, metrics.Duration, w.Header().Get("Content-Type"))
		}
	}
}

// Capture the whole matched route, including authentication and preflight
// responses. A verified identity is returned even when access is denied.
func (s *Server) serveRoute(w http.ResponseWriter, r *http.Request, rt route) *session {
	if routeAllowsAllOrigins(rt) {
		setCORSHeaders(w.Header())
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			serveCORSPreflight(w, r)
			return nil
		}
	}
	if routeUsesOAuth(rt) {
		return s.serveOAuthApp(w, r, rt)
	}
	// Who this is, decided once: the session cookie (a browser) or a sealed
	// CLI token as Bearer. Public routes resolve it too, but only for the
	// gateway's own telemetry and to keep a donkey token away from the app:
	// they never learn who the visitor is.
	sess, fromToken := s.identityFrom(r)
	if rt.Login {
		// The route demands a login. If the installation cannot run one, the
		// promise of privacy wins over availability: fail closed.
		if !s.cfg.authConfigured() {
			s.errorPage(w, http.StatusServiceUnavailable, "This app requires a sign-in, but none is configured.",
				"The installation has no (complete) OIDC setup — `donkey server install` with auth.issuer, clientId and clientSecret fixes this.")
			return sess
		}
		if sess == nil {
			// Browsers get the login round trip; API calls get a plain 401 —
			// a redirect chain into an IdP is useless to a fetch() or a CLI.
			if wantsHTML(r) {
				s.startLogin(w, r)
				return nil
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="donkey"`)
			http.Error(w, `{"error":"sign-in required"}`, http.StatusUnauthorized)
			return nil
		}
		if rt.Personal {
			// The machines' address leads each visitor to their own machine
			// and to no other: which one is decided here, from the sign-in,
			// and from nothing the request says.
			owned, ok := s.machineOf(r.Context(), sess.Subject)
			if !ok {
				s.errorPage(w, http.StatusServiceUnavailable, "Your machine isn't running.",
					"Opening a folder in the donkey console starts it.")
				return sess
			}
			rt = owned
		}
		if !rt.allows(sess.Subject, sess.Email, sess.Groups) {
			who := sess.Email
			if who == "" {
				who = sess.Subject
			}
			if rt.Machine != "" {
				s.errorPage(w, http.StatusForbidden, "This isn't your machine.",
					fmt.Sprintf("You're signed in as %s.", who))
				return sess
			}
			s.deniedPage(w, who)
			return sess
		}
	}
	// The proxy copies response headers with Add. Remove the provisional CORS
	// values set above so ModifyResponse contributes exactly one value on a
	// successful backend response; browsers reject a repeated wildcard.
	if routeAllowsAllOrigins(rt) {
		clearCORSHeaders(w.Header())
	}
	if rt.Machine != "" {
		// A service worker would stay in the browser and answer for this
		// address without asking the gateway again — for whoever signs in
		// there next, too. Browsers name the request that installs one.
		if r.Header.Get("Service-Worker") != "" {
			http.Error(w, "A machine's address takes no service workers.", http.StatusForbidden)
			return sess
		}
		if !fromToken {
			claimBrowser(w, r, rt.Machine)
		}
	}
	idpToken := s.idpToken(w, r, rt, sess, fromToken, time.Time{})
	s.proxy(rt, sess, fromToken, idpToken).ServeHTTP(w, r)
	return sess
}

// machineBrowserCookie names, on the machines' address, the machine a browser
// showed last. Every machine answers on that one address, so a browser files
// what any of them leaves behind — stored drafts, cached scripts — under the
// same origin; this is how the gateway notices that the person in front of it
// is not the one those were left by. The __Host- prefix keeps it to this
// exact host, and it is the gateway's alone: a machine can neither read it
// (HttpOnly) nor set it (see proxy).
const machineBrowserCookie = "__Host-donkey-machine"

// claimBrowser makes a browser start clean when the machine it is about to
// show is not the one it showed last: whatever that one stored under the
// shared address is dropped before the response is used. Without it, a page
// from one person's machine could leave something behind that runs when the
// next person to sign in on the same browser opens theirs.
//
// Only what is stored. Cached responses need no clearing — they are kept per
// sign-in (Vary: Cookie, see proxy), so one person's never answer for
// another — and asking a browser to clear its cache holds the page back
// while it searches all of it, on the first visit of all moments. Cookies
// are left alone too: the directive that clears them would take the
// installation's sign-in with it.
func claimBrowser(w http.ResponseWriter, r *http.Request, machine string) {
	if c, err := r.Cookie(machineBrowserCookie); err == nil && c.Value == machine {
		return
	}
	w.Header().Set("Clear-Site-Data", `"storage"`)
	http.SetCookie(w, &http.Cookie{
		Name: machineBrowserCookie, Value: machine, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 400 * 24 * 60 * 60,
	})
}

// keepBrowserCookie drops a machine's attempt to set the gateway's own note
// about the browser: a page that could write it could claim the browser for
// somebody else's machine in advance.
func keepBrowserCookie(header http.Header) {
	cookies := header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, line := range cookies {
		name, _, _ := strings.Cut(line, "=")
		if strings.EqualFold(strings.TrimSpace(name), machineBrowserCookie) {
			continue
		}
		header.Add("Set-Cookie", line)
	}
}

// consoleHost is where the console answers.
func (s *Server) consoleHost() string {
	if s.cfg.ServerHost != "" {
		return s.cfg.ServerHost
	}
	return s.cfg.Domain
}

// wantsHTML says whether the request came from a navigating browser.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// An app behind a login learns its visitor from X-Forwarded-User (Entra's
// object ID, else the sub), -Email, -Preferred-Username, -Name, -Groups
// and -Access-Token, each also as X-Auth-Request-* (identity.SetHeader;
// X-Auth-Request-User stays the sub); public routes learn nothing, even
// when the domain-wide session cookie arrives with the request. Incoming
// identity headers are dropped on every route — identity must be
// unforgeable, including names donkey never sets but an app may trust
// (identity.StripHeaders).

// accessTokenHeader carries a sealed credential to the one backend that
// verifies identity itself (donkey's own API). Inbound copies are dropped on
// every route, exactly like the identity headers.
const accessTokenHeader = "X-Access-Token"

// idpAccessTokenHeader carries the visitor's IdP access token for donkey's
// own API (auth.scope) to login-protected routes — oauth2-proxy's name for
// it, with X-Auth-Request-Access-Token beside it. Apps hand it on to
// services that exchange it on-behalf-of for their own APIs.
const idpAccessTokenHeader = "X-Forwarded-Access-Token"

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

// machineTransport is the hop to a machine. It waits for one that only just
// came up (runtime.MachineDialer) — the console shows a machine the moment
// it reads "running" — where an app's hop fails at once: an app that does
// not take connections is down, a machine that does not is usually seconds
// from being there.
var machineTransport http.RoundTripper = &http.Transport{
	DialContext:         runtime.MachineDialer(runtime.MachineDialPatience),
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
	ForceAttemptHTTP2:   false,
}

// idpToken is the visitor's IdP access token for a login-protected route, or
// "". Only behind a login: the domain-wide session cookie reaches public
// routes too, and an app that never asked for a sign-in gets no token to
// replay. A failure costs the app its token, never the visitor the page
// (Upstream logs it, once per retry). until is how long the credential
// behind sess can still be presented; zero means sess.Expires.
//
// When the IdP has rotated the refresh token, a browser's session cookie is
// re-sealed with the new one: the browser, not the gateway, keeps it across
// restarts.
func (s *Server) idpToken(w http.ResponseWriter, r *http.Request, rt route, sess *session, fromToken bool, until time.Time) string {
	// A machine runs whatever its owner asks an agent to run: it gets the
	// donkey sign-in it was started with and no IdP token on top.
	if sess == nil || !rt.Login || rt.Machine != "" || !s.upstream.Enabled() {
		return ""
	}
	if sess.RefreshToken == "" {
		s.upstreamMissingToken.Do(func() {
			log.Print("a sign-in has no IdP refresh token: X-Forwarded-Access-Token is omitted; sign out and in again after configuring auth.scope")
		})
		return ""
	}
	if until.IsZero() {
		until = time.Unix(sess.Expires, 0)
	}
	tok, latest, err := s.upstream.AccessToken(r.Context(), sess.Grant, sess.RefreshToken, until)
	if err != nil {
		return ""
	}
	if !fromToken && latest != sess.RefreshToken {
		rotated := *sess
		rotated.RefreshToken = latest
		if err := s.setSession(w, r, &rotated, s.cookieDomain(requestHost(r))); err != nil {
			log.Printf("the session of %s could not take the rotated refresh token: %v", sess.Subject, err)
		}
	}
	return tok
}

// proxy forwards to the app. A ReverseProxy is built per request, so the
// identity serveApp established rides in the closure — nothing is verified
// twice. idpToken is the visitor's IdP access token, if the route gets one;
// Telemetry plugins attach their observation hooks to the request context.
func (s *Server) proxy(rt route, sess *session, fromToken bool, idpToken string) *httputil.ReverseProxy {
	hop := http.RoundTripper(transport)
	if rt.Machine != "" {
		hop = machineTransport
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			for _, observation := range trafficObservations(pr.In) {
				if observation.rewrite != nil {
					observation.rewrite(pr)
				}
			}
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
			identity.StripHeaders(pr.Out.Header)
			pr.Out.Header.Del(accessTokenHeader)
			// A machine's front door only opens for the gateway. The
			// credential is derived, so no route record has to carry it, and
			// a visitor's own copy of the header never travels.
			pr.Out.Header.Del(config.MachineGuardHeader)
			// The control plane's credential at a machine never comes from a
			// browser, so one that claims to is dropped here.
			pr.Out.Header.Del(config.MachineControlHeader)
			if rt.Machine != "" {
				pr.Out.Header.Set(config.MachineGuardHeader, config.MachineGuardToken(s.cfg.SessionKey, rt.Machine))
			}
			// The session cookie is the gateway's, not the app's — don't leak
			// sealed identity material into app code.
			stripCookie(pr.Out, append(sessionCookieNames(), stateCookie, machineBrowserCookie)...)
			// Identity is for the routes that asked for a sign-in.
			if sess != nil && rt.Login {
				identity.SetHeader(pr.Out.Header, "User", sess.Subject)
				// Entra's sub is pairwise per app registration; its object
				// ID is how Graph and downstream APIs know the user. The
				// fallback X-Auth-Request-User keeps the sub it always had.
				identity.SetForwardedHeader(pr.Out.Header, "User", sess.ObjectID)
				identity.SetHeader(pr.Out.Header, "Email", sess.Email)
				identity.SetHeader(pr.Out.Header, "Preferred-Username", sess.PreferredUsername)
				identity.SetHeader(pr.Out.Header, "Name", sess.Name)
				identity.SetHeader(pr.Out.Header, "Groups", strings.Join(sess.Groups, ","))
				identity.SetHeader(pr.Out.Header, "Access-Token", idpToken)
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
					forward := *sess
					forward.Expires, forward.AccessToken = exp, idpToken
					// The API hands the access token on; the refresh token
					// behind it stays with the gateway.
					forward.RefreshToken, forward.Grant = "", ""
					tok, err := identity.SealToken(s.cfg.SessionKey, forward)
					if err == nil {
						pr.Out.Header.Set(accessTokenHeader, tok)
					}
				}
			}
		},
		ModifyResponse: func(res *http.Response) error {
			for _, observation := range trafficObservations(res.Request) {
				if observation.response != nil {
					observation.response(res)
				}
			}
			if routeAllowsAllOrigins(rt) {
				setCORSHeaders(res.Header)
			}
			if rt.Machine != "" {
				s.allowConsoleFrame(res.Header)
				keepBrowserCookie(res.Header)
				// One address, many machines: what a browser cached for one
				// sign-in must not answer for another without asking here.
				res.Header.Add("Vary", "Cookie")
			}
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
		Transport: hop,
		// Flush as bytes arrive: SSE and long-running responses must not sit
		// in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if routeAllowsAllOrigins(rt) {
				setCORSHeaders(w.Header())
			}
			if rt.Machine != "" {
				// The wait is over and the machine still is not there. That is
				// rarely the end of it — a restart under way, a stop the
				// gateway has not read yet — so a page asks again by itself
				// rather than leaving its owner to find the reload button.
				if wantsHTML(r) {
					w.Header().Set("Refresh", "3")
				}
				s.errorPage(w, http.StatusBadGateway, "Your machine isn't answering yet.",
					"It may still be starting, or have just stopped — this page tries again by itself. ("+err.Error()+")")
				return
			}
			s.errorPage(w, http.StatusBadGateway, "The app didn't answer.",
				"It may be starting up or crashed — its logs know more. ("+err.Error()+")")
		},
	}
}

// allowConsoleFrame lets the console, and the machine itself, show a machine
// inside a frame — and nothing else. The machine is its own origin, which is
// what keeps whatever runs in it away from the console's session, so
// embedding has to be granted: to the console, which shows the agent, and to
// the machine, whose agent shows a preview of the owner's files in a frame of
// its own (every ancestor of a frame has to be allowed, not only the top
// one). Added rather than set: a policy the machine sent itself keeps
// applying next to this one.
func (s *Server) allowConsoleFrame(header http.Header) {
	header.Del("X-Frame-Options")
	header.Add("Content-Security-Policy", "frame-ancestors 'self' https://"+s.consoleHost())
}

func setCORSHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Expose-Headers", "MCP-Session-Id, WWW-Authenticate")
}

func clearCORSHeaders(header http.Header) {
	header.Del("Access-Control-Allow-Origin")
	header.Del("Access-Control-Expose-Headers")
}

func serveCORSPreflight(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
		w.Header().Set("Access-Control-Allow-Headers", requested)
	}
	w.WriteHeader(http.StatusNoContent)
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
