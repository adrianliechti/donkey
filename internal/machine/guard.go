// Package machine is what runs inside a personal machine: the front door
// that stands between the gateway and the coding agent, and the small amount
// of housekeeping that turns an image into its owner's environment.
package machine

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

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
}

func (g *Guard) Handler() http.Handler {
	origin := g.Upstream.Scheme + "://" + g.Upstream.Host
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(g.Upstream)
			// The agent only answers to a loopback name, and compares the
			// page's origin with it before taking a WebSocket: both are
			// spelled the way it expects. The public address was checked
			// before the request got here.
			pr.Out.Host = g.Upstream.Host
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", origin)
			}
			// What the gateway said about the visitor ends here. The agent
			// has no use for it, and the owner's own tools must not mistake
			// it for something to trust.
			pr.Out.Header.Del(config.MachineGuardHeader)
			identity.StripHeaders(pr.Out.Header)
		},
		// Agent responses stream; nothing may sit in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "The machine's agent isn't answering yet — it may still be starting.", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == runtime.MachineHealthPath {
			g.health(w)
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
		proxy.ServeHTTP(w, r)
	})
}

func (g *Guard) ownOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, g.Host)
}

// health says whether the agent accepts connections. It is the one answer
// given without the credential, to the runtime that asks whether the machine
// is up, and it says nothing more than that.
func (g *Guard) health(w http.ResponseWriter) {
	conn, err := net.DialTimeout("tcp", g.Upstream.Host, time.Second)
	if err != nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	_ = conn.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
