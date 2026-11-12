package addons

import (
	"os"
	"path"
	"strings"
)

// TunnelAuthorizationHeader carries a local client's own Authorization
// through an HTTP tunnel: between CLI and server, Authorization is donkey's.
const TunnelAuthorizationHeader = "X-Donkey-Tunnel-Authorization"

// Tunnel describes how `donkey addon tunnel` brings a service of one protocol
// to a developer's machine. By default bytes travel to the service's
// in-cluster endpoint untouched. An HTTP tunnel carries requests through
// donkey-server instead, which adds what the service needs and the machine
// lacks — the user's IdP token, the space's key, a proxy grant — and keeps
// the rest of the service out of reach. donkey-server hands it to the CLI
// with the service's binding.
type Tunnel struct {
	// Port is the usual local port; zero means the service's own (bytes) or
	// any free one (requests).
	Port int `json:"port,omitempty"`
	// Hint is a ready-to-run way in; $NAME expands from the local binding,
	// and $URL is an HTTP tunnel's local address.
	Hint string `json:"hint,omitempty"`
	// HTTP carries requests to the binding's URLKey through donkey-server.
	HTTP   bool   `json:"http,omitempty"`
	URLKey string `json:"urlKey,omitempty"`
	// UserToken presents the caller's IdP access token as Bearer.
	UserToken bool `json:"userToken,omitempty"`
	// Links are top-level JSON response fields holding absolute links to the
	// service; the CLI points them back at the tunnel.
	Links []string `json:"links,omitempty"`

	// The rest stays server-side. Paths pass, each with everything below it;
	// none means all. DropHeaders are client headers the service must not
	// see.
	Paths       []string `json:"-"`
	DropHeaders []string `json:"-"`
	// Credential is a header donkey-server adds, "Name: value" with $KEY from
	// the binding. A binding without that entry was attached from elsewhere
	// and is reached at its own address, not through donkey.
	Credential string `json:"-"`
	// ProxyGrant authenticates like the console does to a configured proxy
	// service, which otherwise knows its callers by their pod's address.
	ProxyGrant bool `json:"-"`
	// Destination: the protocol's clients say where a MOVE or COPY goes by the
	// tunnel's own address; donkey-server points that at the service.
	Destination bool `json:"-"`
}

var tunnels = map[string]Tunnel{
	ProtocolPostgres: {Port: 5432, Hint: `psql "$DATABASE_URL"`},
	ProtocolMySQL:    {Port: 3306},
	ProtocolRedis:    {Port: 6379, Hint: `redis-cli -u "$REDIS_URL"`},
	ProtocolS3:       {Port: 9000},
	// Trino checks who runs a query: donkey adds the user's token, only the
	// statement API passes, a typed X-Trino-User could only impersonate, and
	// continuation links must lead back through the tunnel. Without a data
	// encoding the answer stays inline: spooled segments would be fetched
	// from elsewhere, around the tunnel.
	ProtocolTrino: {
		Port: 8080, Hint: `trino --server "$URL"`,
		HTTP: true, UserToken: true,
		Paths:       []string{"/v1/statement"},
		DropHeaders: []string{"X-Trino-User", "X-Trino-Original-User", "X-Trino-Original-Roles", "X-Trino-Query-Data-Encoding"},
		Links:       []string{"nextUri", "partialCancelUri"},
	},
	// The platform's AI and search proxies take the space's key, which the
	// binding holds; they decide themselves which operations pass.
	ProtocolOpenAI: {
		Port: 8081, Hint: `curl "$URL/models"`,
		HTTP: true, Credential: "Authorization: Bearer $OPENAI_API_KEY",
	},
	ProtocolExa: {
		Port: 8082, Hint: `curl "$URL/search" -H "Content-Type: application/json" -d '{"query":"donkey"}'`,
		HTTP: true, Credential: "x-api-key: $EXA_API_KEY",
	},
	// A WebDAV service checks who asks, like Trino: donkey adds the user's
	// token, and every path passes — the tree is the service. MOVE and COPY
	// name their destination by the tunnel's address; donkey-server points it
	// at the service, and the CLI points hrefs and Locations back at the tunnel.
	ProtocolWebDAV: {
		Port: 8083, Hint: `curl -X PROPFIND "$URL" -H "Depth: 1"`,
		HTTP: true, UserToken: true,
		Destination: true,
	},
	// A configured proxy service: any free port, as a space may use several.
	ProtocolHTTP: {Hint: `curl "$URL"`, HTTP: true, ProxyGrant: true},
}

// TunnelFor is the tunnel of a service of this type and protocol; an unknown
// protocol carries bytes to the service's own port.
func TunnelFor(typ, protocol string) Tunnel {
	protocol = NormalizeProtocol(protocol)
	t := tunnels[protocol]
	if t.HTTP {
		t.URLKey = ConnectionKey(protocol)
		if protocol == ProtocolHTTP {
			t.URLKey = ProxyEnvKey(typ)
		}
	}
	return t
}

// HTTPTunnelProtocols are the protocols whose tunnel carries requests.
func HTTPTunnelProtocols() []string {
	var out []string
	for protocol, t := range tunnels {
		if t.HTTP {
			out = append(out, protocol)
		}
	}
	return out
}

// Allows reports whether a request path may pass the tunnel.
func (t Tunnel) Allows(p string) bool {
	if len(t.Paths) == 0 {
		return true
	}
	p = path.Clean("/" + p)
	for _, allowed := range t.Paths {
		if p == allowed || strings.HasPrefix(p, allowed+"/") {
			return true
		}
	}
	return false
}

// CredentialHeader renders Credential from the binding. ok is false when an
// entry it needs is missing — a service attached from elsewhere.
func (t Tunnel) CredentialHeader(binding map[string]string) (name, value string, ok bool) {
	if t.Credential == "" {
		return "", "", true
	}
	name, template, _ := strings.Cut(t.Credential, ": ")
	ok = true
	value = os.Expand(template, func(key string) string {
		ok = ok && binding[key] != ""
		return binding[key]
	})
	return name, value, ok
}
