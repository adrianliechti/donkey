package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// RouteEntry is one extra address a request can reach the app on.
type RouteEntry struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

// NetworkResponse is what the console's network settings show: read from the
// runtime's actual state, never from stored intent.
type NetworkResponse struct {
	// Mode is web or api. Exposure is public, protected or internal.
	Mode     string `json:"mode"`
	Exposure string `json:"exposure"`
	URL      string `json:"url,omitempty"`
	// DefaultHost is the generated <app>.<space>.<domain> address. Host is the
	// configured donkey.yaml shape when it differs: a name, "@", or a full
	// hostname.
	DefaultHost string       `json:"defaultHost"`
	FQDN        string       `json:"fqdn"`
	Domains     []string     `json:"domains"`
	Host        string       `json:"host,omitempty"`
	Port        int          `json:"port,omitempty"`
	Routes      []RouteEntry `json:"routes,omitempty"`
	AllowPublic bool         `json:"allowPublic"`
	CORS        string       `json:"cors"`
	// Analytics is false when the app opted out of web analytics. It only
	// counts for web apps with an address; see config.AppConfig.CountsVisits.
	Analytics bool `json:"analytics"`
}

func (s *Server) networkResponse(r *http.Request, space, app string) (*NetworkResponse, error) {
	st, err := s.rt.Status(r.Context(), space, app)
	if err != nil {
		return nil, err
	}
	if !st.Found {
		return nil, nil
	}
	net, err := s.rt.Network(r.Context(), space, app)
	if err != nil {
		return nil, err
	}
	settings, err := s.spaceSettings(r.Context(), space)
	if err != nil {
		return nil, err
	}
	resp := &NetworkResponse{
		FQDN:        net.Host,
		Domains:     append([]string{}, settings.Domains...),
		DefaultHost: config.AppHost(&config.AppConfig{Name: app}, space, s.cfg.Domain),
		Mode:        modeOfStatus(st),
		Port:        int(st.Port),
		AllowPublic: settings.AllowPublic,
		CORS:        st.CORS,
		Analytics:   net.Analytics != config.AnalyticsOff,
	}
	if net.Mode != "" {
		resp.Mode = net.Mode
	}
	if net.RouteFound {
		resp.CORS = net.CORS
	}
	switch {
	case !net.RouteFound:
		resp.Exposure = "internal"
	case net.Protected:
		resp.Exposure = "protected"
	default:
		resp.Exposure = "public"
	}
	if net.Host != "" {
		resp.URL = s.cfg.Scheme() + "://" + net.Host
		resp.Host = hostShape(net.Host, resp.DefaultHost, space, s.cfg.Domain)
	}
	for _, pr := range net.Routes {
		resp.Routes = append(resp.Routes, RouteEntry{Host: pr.Host, Path: pr.Path})
	}
	return resp, nil
}

// hostShape turns the hostname an app actually serves on back into the
// donkey.yaml `host:` value that would produce it — "" for the default,
// "@" for the space's own address, a bare label for one of its own, and the
// hostname itself for anything else. Without this the console would read
// "@" back as a literal hostname and the app would stop following its space.
func hostShape(host, defaultHost, space, domain string) string {
	spaceSuffix := "." + space + "." + domain
	switch {
	case host == defaultHost:
		return ""
	case host == space+"."+domain:
		return config.RootHost
	case strings.HasSuffix(host, spaceSuffix):
		return strings.TrimSuffix(host, spaceSuffix)
	default:
		return host
	}
}

func (s *Server) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp, err := s.networkResponse(r, space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if resp == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("nothing with this name is deployed"))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type PutNetworkRequest struct {
	Mode     string `json:"mode"`
	Exposure string `json:"exposure"`
	CORS     string `json:"cors"`
	// Analytics turns web analytics on or off; omitted keeps what the app has.
	Analytics *bool `json:"analytics"`
	// Host is where the app should answer instead of its default
	// address — see config.AppHost: a plain name sits under the space, "@"
	// is the space's own address, anything with a dot is an exact hostname,
	// and empty puts it back on the default.
	Host string `json:"host"`
}

// handlePutNetwork changes how an app is reached — without a deploy. The
// runtime re-renders the app's addresses from its actual state; the choice is
// recorded in the console metadata so later deploys keep it (unless
// donkey.yaml itself says otherwise, which always wins).
func (s *Server) handlePutNetwork(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req PutNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Mode == "" {
		req.Mode = config.AppModeWeb
	}
	if !slices.Contains([]string{"public", "protected", "internal"}, req.Exposure) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("exposure must be public, protected or internal"))
		return
	}
	if req.Mode != config.AppModeWeb && req.Mode != config.AppModeAPI {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("mode must be web or api"))
		return
	}
	if req.CORS != "" && req.CORS != config.CORSAllowAll {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("cors must be allowAll when set"))
		return
	}

	ctx := r.Context()
	st, err := s.rt.Status(ctx, space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !st.Found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("nothing with this name is deployed"))
		return
	}
	if st.Port == 0 {
		writeErr(w, http.StatusConflict, fmt.Errorf("the app's port can't be read from the cluster — deploy it again first"))
		return
	}
	net, err := s.rt.Network(ctx, space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if req.Exposure == "internal" && len(net.Routes) > 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("an internal app cannot have gateway routes — remove its shared routes before changing exposure"))
		return
	}
	console := s.readConsoleChoices(ctx, space, app)
	analytics := req.Analytics
	if analytics == nil {
		on := net.Analytics != config.AnalyticsOff
		analytics = &on
	}

	// A synthetic app config carries the choice through the same validation
	// and rendering a donkey.yaml would.
	cfg := &config.AppConfig{Name: app, Space: space, Port: int(st.Port)}
	// Network edits preserve the explicit OIDC registration from donkey.yaml.
	cfg.OIDC = net.OIDC
	if req.Mode != config.AppModeWeb {
		cfg.Mode = req.Mode
	}
	switch req.Exposure {
	case "public":
		cfg.Public = true
	case "internal":
		cfg.Internal = true
	}
	cfg.Host = strings.TrimSpace(req.Host)
	cfg.CORS = req.CORS
	if !*analytics {
		cfg.Analytics = analytics
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := runtime.CheckAppDomain(ctx, s.rt, cfg, space, s.cfg.Domain); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	note, err := s.rt.ApplyNetwork(ctx, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// Record the choice so deploys keep it while donkey.yaml stays silent.
	console.Exposure = req.Exposure
	console.Mode = req.Mode
	console.Host = cfg.Host
	console.CORS = cfg.CORS
	console.Analytics = config.RouteAnalytics(cfg)
	if err := s.writeConsoleChoices(ctx, space, app, console); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	resp, err := s.networkResponse(r, space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"network": resp, "note": note})
}

// writeConsoleChoices stores the console's network choices with the deployed
// app — as app metadata the deploy pipeline never rewrites, so they survive
// deploys untouched.
func (s *Server) writeConsoleChoices(ctx context.Context, space, app string, c consoleChoices) error {
	var value *string
	if c != (consoleChoices{}) {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		v := string(b)
		value = &v
	}
	return s.rt.SetAppMeta(ctx, space, app, runtime.MetaConsole, value)
}
