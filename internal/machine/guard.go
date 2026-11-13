// Package machine is what runs inside a personal machine: the front door
// that stands between the gateway and the coding agent, and the small amount
// of housekeeping that turns an image into its owner's environment.
package machine

import (
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// Guard is the machine's front door. The agent behind it trusts whoever
// reaches it — it was written to listen on a developer's loopback — so the
// guard is where that trust is earned: it admits only requests carrying the
// credential the gateway presents, and the gateway presents it only after
// the machine's owner has signed in.
//
// Reaching the container is therefore not enough. A neighbour on the same
// network, or a pod in a cluster whose network plugin ignores policies, gets
// a 403 and nothing else.
type Guard struct {
	// Token is the credential the gateway presents (config.MachineGuardToken).
	Token string
	// Host is the machine's public hostname.
	Host string
	// Upstream is the agent's loopback address.
	Upstream *url.URL
	// ControlToken is the control plane's own credential
	// (config.MachineControlToken), and Control what it reaches with it:
	// everything under ControlPrefix. The gateway's credential does not open
	// it, so nothing a browser sends ever arrives there.
	ControlToken string
	Control      http.Handler
	// Used is told of everything that comes through the door for the owner —
	// what keeps an idle machine from being taken for a used one, and the
	// other way around.
	Used func()
	// Reserved are loopback ports the door keeps to itself (the model relay).
	Reserved []string
}

// PortPrefix is where the machine's other loopback ports answer, on the
// machine's own address: /.donkey/port/<port>/… reaches 127.0.0.1:<port>.
//
// It exists for what the agent serves beside itself. Its HTML preview comes
// from a second loopback port, and its redirect there would send the owner's
// browser to the owner's own computer; the door rewrites that redirect onto
// this path instead (see publicLocation). A server the owner starts in the
// machine is reachable the same way.
//
// Everything under it is the same origin as the agent's interface. A page
// served from here can do in the machine what its owner can: previewing a
// file runs it, the way building it would. A path prefix also cannot stand
// in for a root — a page that asks for /assets/x.js asks the agent — so a
// server that needs its own root wants an address of its own, which machines
// do not have yet.
const PortPrefix = "/.donkey/port/"

func (g *Guard) Handler() http.Handler {
	agent := g.proxy(g.Upstream, "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == runtime.MachineHealthPath {
			g.health(w)
			return
		}
		if strings.HasPrefix(r.URL.Path, ControlPrefix) {
			if g.Control == nil || g.ControlToken == "" ||
				subtle.ConstantTimeCompare([]byte(r.Header.Get(config.MachineControlHeader)), []byte(g.ControlToken)) != 1 {
				http.Error(w, "This is for donkey itself.", http.StatusForbidden)
				return
			}
			g.Control.ServeHTTP(w, r)
			return
		}
		if g.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(config.MachineGuardHeader)), []byte(g.Token)) != 1 {
			http.Error(w, "This machine only answers through its donkey address.", http.StatusForbidden)
			return
		}
		// A page of another site must not drive the agent with the owner's
		// session. Browsers name the page a request comes from; one that is
		// not this machine's own is refused here, before it is rewritten.
		if origin := r.Header.Get("Origin"); origin != "" && !g.ownOrigin(origin) {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		if g.Used != nil {
			g.Used()
		}
		if strings.HasPrefix(r.URL.Path, PortPrefix) {
			port, ok := portOf(r.URL.Path)
			// The agent has an address already. Under this one its pages
			// would ask for their scripts at the wrong place.
			if !ok || port == g.Upstream.Port() || slices.Contains(g.Reserved, port) {
				http.NotFound(w, r)
				return
			}
			upstream := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
			g.proxy(upstream, PortPrefix+port).ServeHTTP(w, r)
			return
		}
		agent.ServeHTTP(w, r)
	})
}

// portOf reads the port of a /.donkey/port/<port>/… path. Only the plain
// spelling counts, so one port has one address.
func portOf(path string) (string, bool) {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, PortPrefix), "/")
	port, err := strconv.Atoi(segment)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != segment {
		return "", false
	}
	return segment, true
}

// proxy forwards to one loopback listener of the machine: the agent, or —
// with the path prefix it answers under — another port.
func (g *Guard) proxy(upstream *url.URL, prefix string) *httputil.ReverseProxy {
	origin := upstream.Scheme + "://" + upstream.Host
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = upstream.Scheme, upstream.Host
			if prefix != "" {
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
				if pr.Out.URL.Path == "" {
					pr.Out.URL.Path = "/"
				}
				// The rest as the browser wrote it, so an escaped character
				// arrives escaped. A prefix that was itself written with
				// escapes leaves the spelling to the decoded path.
				pr.Out.URL.RawPath = ""
				if escaped, ok := strings.CutPrefix(pr.In.URL.EscapedPath(), prefix); ok && escaped != "" {
					pr.Out.URL.RawPath = escaped
				}
			}
			// The agent only answers to a loopback name, and compares the
			// page's origin with it before taking a WebSocket: both are
			// spelled the way it expects. The public address was checked
			// before the request got here.
			pr.Out.Host = upstream.Host
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", origin)
			}
			// What the gateway said about the visitor ends here. Nothing in
			// the machine has a use for it, and the owner's own tools must
			// not mistake it for something to trust.
			pr.Out.Header.Del(config.MachineGuardHeader)
			identity.StripHeaders(pr.Out.Header)
		},
		ModifyResponse: func(res *http.Response) error {
			// A connection that stays open — a terminal, the agent's stream —
			// is used whenever something passes through it.
			if stream, ok := res.Body.(io.ReadWriteCloser); ok && res.StatusCode == http.StatusSwitchingProtocols && g.Used != nil {
				res.Body = usedStream{stream, g.Used}
			}
			if location := res.Header.Get("Location"); location != "" {
				res.Header.Set("Location", g.publicLocation(location, prefix))
			}
			if prefix != "" {
				scopeCookies(res, prefix)
			}
			return nil
		},
		// Agent responses stream; nothing may sit in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if prefix != "" {
				http.Error(w, "Nothing in the machine is listening on port "+upstream.Port()+".", http.StatusBadGateway)
				return
			}
			http.Error(w, "Nothing is open in this machine yet, or its agent is still starting. Folders are opened on the Machine page of the donkey console.", http.StatusBadGateway)
		},
	}
}

// publicLocation turns a redirect written inside the machine into one the
// owner's browser can follow. Software in the machine names itself by its
// loopback address, which in the owner's browser is the owner's own
// computer: such a redirect is moved onto the machine's address, under the
// port's path. A path from the root stays with the listener that wrote it.
// Anything else — a relative path, another site — is the browser's to read.
func (g *Guard) publicLocation(location, prefix string) string {
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	if u.Host == "" {
		if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
			return prefix + location
		}
		return location
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || u.Port() == "" || !(u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return location
	}
	rest := u.RequestURI()
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if u.Fragment != "" {
		rest += "#" + u.EscapedFragment()
	}
	if u.Port() == g.Upstream.Port() {
		return rest
	}
	return PortPrefix + u.Port() + rest
}

// scopeCookies keeps what a port's listener stores in the browser under that
// port's path: it shares the machine's address with the agent and with every
// other port, and a cookie set for "/" would travel to all of them.
func scopeCookies(res *http.Response, prefix string) {
	cookies := res.Cookies()
	if len(cookies) == 0 {
		return
	}
	res.Header.Del("Set-Cookie")
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Path, "/") {
			cookie.Path = "/"
		}
		cookie.Path = prefix + cookie.Path
		res.Header.Add("Set-Cookie", cookie.String())
	}
}

func (g *Guard) ownOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, g.Host)
}

// health says the door is up. It is the one answer given without a
// credential, to the runtime that asks whether the machine can take requests,
// and it says nothing more than that — whether a folder is open is the
// control plane's to ask.
func (g *Guard) health(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
