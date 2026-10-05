// Package serviceproxy exposes configured HTTP targets through space-owned
// bindings. Apps use their runtime network identity; their Authorization header
// remains available to the upstream API.
package serviceproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

type Access func(context.Context, string, string) (bool, error)

type route struct{ prefix, path, rawPath, host string }
type routeKey struct{}

// Handler joins each request's suffix and query onto its configured target.
// proxyUrl selects an outbound proxy; omission means a direct connection.
func Handler(services config.ProxyServices, access Access) (http.Handler, error) {
	if err := services.Validate(); err != nil {
		return nil, err
	}
	if access == nil {
		return nil, fmt.Errorf("proxy service access resolver is required")
	}
	proxies := map[string]*httputil.ReverseProxy{}
	for _, service := range services {
		target, _ := url.Parse(service.TargetURL)
		credentials := target.User
		target.User = nil
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		if service.ProxyURL != "" {
			outbound, _ := url.Parse(service.ProxyURL)
			transport.Proxy = http.ProxyURL(outbound)
		}
		proxies[service.Name] = &httputil.ReverseProxy{
			Transport: transport, FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.Header.Del(TestRequestHeader)
				path := pr.In.Context().Value(routeKey{}).(route)
				pr.Out.URL.Path, pr.Out.URL.RawPath = path.path, path.rawPath
				pr.SetURL(target)
				// SetURL joins with a slash even for an empty suffix. Preserve
				// the exact base URL so an upstream's slash redirect terminates.
				if path.rawPath == "" {
					pr.Out.URL.Path, pr.Out.URL.RawPath = target.Path, target.RawPath
				}
				pr.Out.URL.ForceQuery = pr.In.URL.ForceQuery || target.ForceQuery
				pr.Out.Host = target.Host
				if credentials != nil {
					password, _ := credentials.Password()
					pr.Out.SetBasicAuth(credentials.Username(), password)
				}
				// Do not send internal binding addresses to the target through
				// browser navigation headers.
				if referer, err := url.Parse(pr.Out.Header.Get("Referer")); err == nil && referer.Host == path.host {
					pr.Out.Header.Del("Referer")
				}
			},
			ModifyResponse: func(response *http.Response) error {
				if response.Request == nil {
					return nil
				}
				path := response.Request.Context().Value(routeKey{}).(route)
				if raw := response.Header.Get("Location"); raw != "" {
					if location, err := url.Parse(raw); err == nil {
						location = response.Request.URL.ResolveReference(location)
						if strings.EqualFold(location.Host, target.Host) && location.Scheme == target.Scheme {
							if suffix, ok := targetSuffix(target.EscapedPath(), location.EscapedPath()); ok {
								location.Scheme, location.Host, location.User = "", "", nil
								location.RawPath = path.prefix + suffix
								location.Path, _ = url.PathUnescape(location.RawPath)
							}
						}
						// A redirect outside the configured base keeps its upstream
						// destination instead of resolving against the binding URL.
						response.Header.Set("Location", location.String())
					}
				}
				cookies := response.Cookies()
				if len(cookies) > 0 {
					response.Header.Del("Set-Cookie")
					for _, cookie := range cookies {
						suffix, ok := targetSuffix(target.Path, cookie.Path)
						if !ok {
							suffix = "/"
						}
						cookie.Domain, cookie.Path = "", path.prefix+suffix
						response.Header.Add("Set-Cookie", cookie.String())
					}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "proxy target unavailable", http.StatusBadGateway)
			},
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/", 3)
		if len(parts) < 2 || parts[0] != "services" || proxies[parts[1]] == nil {
			http.NotFound(w, r)
			return
		}
		id := parts[1]
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || ip.Zone() != "" {
			http.Error(w, "unknown service caller", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		allowed, err := access(ctx, ip.Unmap().String(), id)
		cancel()
		if err != nil {
			http.Error(w, "service access unavailable", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, "service is not enabled for this caller", http.StatusForbidden)
			return
		}
		rawPath := ""
		if len(parts) == 3 {
			rawPath = "/" + parts[2]
		}
		if !cleanSuffix(rawPath) {
			http.Error(w, "invalid service path", http.StatusBadRequest)
			return
		}
		path, err := url.PathUnescape(rawPath)
		if err != nil {
			http.Error(w, "invalid service path", http.StatusBadRequest)
			return
		}
		forward := route{prefix: "/services/" + id, path: path, rawPath: rawPath, host: r.Host}
		proxies[id].ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, forward)))
	}), nil
}

func targetSuffix(base, path string) (string, bool) {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return path, true
	}
	if path == base {
		return "", true
	}
	if strings.HasPrefix(path, base+"/") {
		return strings.TrimPrefix(path, base), true
	}
	return "", false
}

// Repeated decoding cannot let an upstream normalize a suffix out of the
// configured base path. Encoded slashes and Unicode otherwise stay intact.
func cleanSuffix(path string) bool {
	for {
		for _, part := range strings.Split(path, "/") {
			if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
				return false
			}
		}
		next, err := url.PathUnescape(path)
		if err != nil {
			// A decoded literal percent is valid (for example an ID with
			// "%25"). The original URL is still checked by PathUnescape.
			return true
		}
		if next == path {
			return true
		}
		path = next
	}
}
