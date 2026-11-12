// Command private-packages is the authenticated TLS package mirror used by
// donkey's live E2E installations. It deliberately does no caching: it proves
// donkey-packages supplies credentials, then forwards each protocol to its
// public reference service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type route struct {
	prefix   string
	upstream *url.URL
}

func main() {
	listen := envOr("LISTEN", ":8443")
	username, password := envOr("USERNAME", "e2e-reader"), envOr("PASSWORD", "e2e-token")
	routes := []route{
		newRoute("/npm", "https://registry.npmjs.org"),
		newRoute("/pypi", "https://pypi.org/simple"),
		newRoute("/go", "https://proxy.golang.org"),
		newRoute("/maven", "https://repo.maven.apache.org/maven2"),
		newRoute("/nuget", "https://api.nuget.org/v3"),
		newRoute("/debian", "https://deb.debian.org/debian"),
		newRoute("/debian-unsigned", "https://deb.debian.org/debian"),
		newRoute("/alpine", "https://dl-cdn.alpinelinux.org/alpine"),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			route := pr.In.Context().Value(routeKey{}).(route)
			rest := strings.TrimPrefix(pr.In.URL.EscapedPath(), route.prefix)
			pr.Out.URL = join(route.upstream, rest)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = route.upstream.Host
			pr.Out.Header.Del("Authorization")
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		if !ok || u != username || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="donkey-e2e-packages"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		for _, candidate := range routes {
			if req.URL.EscapedPath() == candidate.prefix || strings.HasPrefix(req.URL.EscapedPath(), candidate.prefix+"/") {
				// Keep a valid Release file and its hashes, but omit signatures
				// so tests exercise both APT opt-ins for unsigned repositories.
				if candidate.prefix == "/debian-unsigned" &&
					(strings.HasSuffix(req.URL.Path, "/InRelease") || strings.HasSuffix(req.URL.Path, "/Release.gpg")) {
					http.NotFound(w, req)
					return
				}
				ctx := context.WithValue(req.Context(), routeKey{}, candidate)
				proxy.ServeHTTP(w, req.WithContext(ctx))
				return
			}
		}
		http.NotFound(w, req)
	})

	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("private package mirror listening on %s", listen)
	err := server.ListenAndServeTLS(required("TLS_CERT"), required("TLS_KEY"))
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type routeKey struct{}

func newRoute(prefix, raw string) route {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return route{prefix: prefix, upstream: u}
}

func join(base *url.URL, rest string) *url.URL {
	target := *base
	escaped := strings.TrimRight(base.EscapedPath(), "/") + "/" + strings.TrimLeft(rest, "/")
	target.Path, _ = url.PathUnescape(escaped)
	target.RawPath = escaped
	return &target
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", name)
		os.Exit(2)
	}
	return value
}
