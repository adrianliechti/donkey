package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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
			if res.StatusCode == http.StatusUnauthorized {
				refusedCredential(res)
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

// refusedCredential turns the provider's 401 into a 502. To the console a 401
// means its own session expired, and it would send the admin to sign in again
// — over a key that belongs to the service, not to them. The provider's
// reason is kept; OpenAI and Exa both put it under "error".
func refusedCredential(res *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
	reason := ""
	var shaped struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &shaped) == nil && len(shaped.Error) > 0 {
		var text string
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(shaped.Error, &text) == nil {
			reason = text
		} else if json.Unmarshal(shaped.Error, &nested) == nil {
			reason = nested.Message
		}
	}
	message := "the service endpoint refused the service's credential"
	if reason = strings.TrimSpace(reason); reason != "" {
		message += ": " + reason
	}
	body, _ := json.Marshal(map[string]string{"error": message})
	res.StatusCode, res.Status = http.StatusBadGateway, "502 Bad Gateway"
	res.Header.Del("WWW-Authenticate")
	res.Header.Del("Content-Encoding")
	res.Header.Set("Content-Type", "application/json")
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	res.ContentLength = int64(len(body))
	res.Body = io.NopCloser(bytes.NewReader(body))
}
