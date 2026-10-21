package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
)

func (s *Server) handleServiceAI(w http.ResponseWriter, r *http.Request) {
	operation := "models"
	if r.Method == http.MethodPost {
		operation = "chat/completions"
	}
	s.forwardServiceAPI(w, r, addons.ProtocolOpenAI, operation)
}

func (s *Server) handleServiceExa(w http.ResponseWriter, r *http.Request) {
	operation := "search"
	if strings.HasSuffix(r.URL.Path, "/contents") {
		operation = "contents"
	}
	s.forwardServiceAPI(w, r, addons.ProtocolExa, operation)
}

// Both consoles use only the saved service endpoint and its credential.
// Browser session headers never cross the provider boundary.
func (s *Server) forwardServiceAPI(w http.ResponseWriter, r *http.Request, protocol, operation string) {
	service, err := s.serviceConnection(r.Context(), r, protocol)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	base, _ := url.Parse(service.Binding[addons.ConnectionKey(protocol)])
	// JoinPath on an empty path yields a relative one ("search"), which the
	// proxy would send as is in the request line.
	if base.Path == "" {
		base.Path = "/"
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = base.JoinPath(operation)
			pr.Out.Host = base.Host
			pr.Out.Header = make(http.Header)
			pr.Out.Header.Set("Content-Type", "application/json")
			pr.Out.Header.Set("Accept", pr.In.Header.Get("Accept"))
			if protocol == addons.ProtocolOpenAI {
				if key := service.Binding["OPENAI_API_KEY"]; key != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+key)
				}
			} else if key := service.Binding["EXA_API_KEY"]; key != "" {
				pr.Out.Header.Set("x-api-key", key)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if protocol != addons.ProtocolOpenAI || r.Method != http.MethodPost || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				res.Header.Set("Content-Type", "application/json")
			}
			res.Header.Set("X-Content-Type-Options", "nosniff")
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				return fmt.Errorf("the service endpoint redirected the request")
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the service endpoint did not answer"))
		},
	}
	proxy.ServeHTTP(w, r)
}
