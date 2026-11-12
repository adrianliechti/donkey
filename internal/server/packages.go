package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianliechti/donkey/internal/identity"
)

// PackagesAddressHeader carries the address `donkey packages` listens on at
// the developer's machine. The mirror writes the links in its answers — the
// tarballs in npm metadata, the files of a PyPI index — to its own address,
// and that local one is the only address the developer's package manager can
// follow.
const PackagesAddressHeader = "X-Donkey-Packages-Address"

const packagesRoutePrefix = "/v1/packages"

// packageEcosystems are the mirror's routes a developer machine gets. The OS
// archives stay with builds: they exist for base images, not for laptops.
var packageEcosystems = map[string]bool{"npm": true, "pypi": true, "go": true, "maven": true, "nuget": true}

// packagesHandler lets a signed-in developer's machine install through the
// package mirror builds use, for networks where the public registries are
// blocked on the machine but reachable from the installation. It forwards to
// donkey-packages, which keeps the upstream credentials to itself exactly as
// it does for a build.
//
// Any verified sign-in is admitted, and nothing but the seal is checked: an
// install is hundreds of requests, and reading the platform settings for
// each — as auth does — would spend the cluster API budget every other call
// shares.
func (s *Server) packagesHandler() http.Handler {
	base, err := url.Parse(s.cfg.PackagesURL)
	if err != nil || base.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable,
				fmt.Errorf("the package mirror address %q is not a URL (DONKEY_PACKAGES_URL)", s.cfg.PackagesURL))
		})
	}
	root := strings.TrimRight(base.EscapedPath(), "/")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = base.Scheme, base.Host
			// The path as the package manager wrote it: a scoped npm name
			// arrives as @scope%2fname and has to leave that way.
			escaped := root + strings.TrimPrefix(pr.In.URL.EscapedPath(), packagesRoutePrefix)
			pr.Out.URL.Path, _ = url.PathUnescape(escaped)
			pr.Out.URL.RawPath = escaped
			// The mirror answers with links to the host it was asked as.
			pr.Out.Host = pr.In.Header.Get(PackagesAddressHeader)
			pr.Out.Header.Del(PackagesAddressHeader)
			// The sign-in has done its work here, and the mirror takes no
			// credential from a caller.
			identity.StripHeaders(pr.Out.Header)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Access-Token")
			pr.Out.Header.Del("Cookie")
		},
		// In-cluster, like the hop to the orchestrator: never through the
		// installation's forward proxy, and no deadline on a large download.
		Transport:      orchestratorTransport,
		ModifyResponse: compressPackageMetadata,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				log.Printf("forwarding %s %s to donkey-packages: %v", r.Method, r.URL.Path, err)
			}
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the package mirror (donkey-packages) isn't answering — try again in a moment"))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := identity.VerifyToken(s.cfg.SessionKey, callerToken(r)); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in — run `donkey login`"})
			return
		}
		// The bare route is what `donkey packages` asks before a package
		// manager does: is the mirror offered here, and is the sign-in good.
		// An older server answers it with the console's page instead.
		if r.PathValue("path") == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		ecosystem, _, _ := strings.Cut(r.PathValue("path"), "/")
		if !packageEcosystems[ecosystem] {
			writeErr(w, http.StatusNotFound, fmt.Errorf("the package mirror serves npm, pypi, go, maven and nuget"))
			return
		}
		if !loopbackAddress(r.Header.Get(PackagesAddressHeader)) {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("the package mirror is reached through `donkey packages`, which names its local address in %s", PackagesAddressHeader))
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// loopbackAddress reports whether value is a host:port on the caller's own
// machine. It becomes the host in the links the mirror writes, so it is
// never taken as an arbitrary name.
func loopbackAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// compressPackageMetadata gzips package indexes on their way out of the
// cluster. The mirror serves them uncompressed so it can rewrite their
// links, which is fine next door to a build and slow over a VPN: npm's
// install metadata for a package like next is 25 MB plain and 2 MB
// compressed. Archives are compressed already and pass as they are.
func compressPackageMetadata(res *http.Response) error {
	if res.StatusCode != http.StatusOK || res.Request.Method == http.MethodHead ||
		res.Header.Get("Content-Encoding") != "" ||
		!strings.Contains(strings.ToLower(res.Request.Header.Get("Accept-Encoding")), "gzip") {
		return nil
	}
	contentType := res.Header.Get("Content-Type")
	if !strings.Contains(contentType, "json") && !strings.Contains(contentType, "text") &&
		!strings.Contains(contentType, "html") && !strings.Contains(contentType, "xml") {
		return nil
	}
	plain := res.Body
	reader, writer := io.Pipe()
	go func() {
		defer plain.Close()
		compressed, _ := gzip.NewWriterLevel(writer, gzip.BestSpeed)
		_, err := io.Copy(compressed, plain)
		if err == nil {
			err = compressed.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	res.Body = &compressedBody{PipeReader: reader, plain: plain}
	res.ContentLength = -1
	res.Header.Del("Content-Length")
	res.Header.Set("Content-Encoding", "gzip")
	res.Header.Add("Vary", "Accept-Encoding")
	return nil
}

type compressedBody struct {
	*io.PipeReader
	plain io.ReadCloser
}

func (b *compressedBody) Close() error {
	// A caller that went away must release both ends: the pending write of
	// compressed bytes and the pending read from the mirror.
	_ = b.PipeReader.Close()
	return b.plain.Close()
}
