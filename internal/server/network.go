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
	DefaultHost string               `json:"defaultHost"`
	FQDN        string               `json:"fqdn"`
	Domains     []string             `json:"domains"`
	Host        string               `json:"host,omitempty"`
	Port        int                  `json:"port,omitempty"`
	Routes      []RouteEntry         `json:"routes,omitempty"`
	Access      *config.AccessConfig `json:"access,omitempty"`
	CORS        string               `json:"cors"`
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
		Access:      net.Access,
		CORS:        st.CORS,
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

	unlock := s.lockApp(space, app)
	defer unlock()

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
	console := s.readConsoleChoices(ctx, space, app)

	// A synthetic app config carries the choice through the same validation
	// and rendering a donkey.yaml would.
	cfg := &config.AppConfig{Name: app, Space: space, Port: int(st.Port)}
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
	if req.Exposure == "protected" {
		// Access rules ride along unchanged; runtime state is the truth,
		// with the console's memory as fallback (e.g. internal → protected).
		cfg.Access = net.Access
		if cfg.Access == nil {
			cfg.Access = console.Access
		}
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

type PutAccessRequest struct {
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
	Roles  []string `json:"roles"`
}

// handlePutAccess narrows (or reopens) who may open a login-protected app.
// The gateway picks it up within seconds — no deploy, no restart.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req PutAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	access := &config.AccessConfig{Users: cleanList(req.Users), Groups: cleanList(req.Groups), Roles: cleanList(req.Roles)}
	if err := access.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if access.Empty() {
		access = nil
	}

	unlock := s.lockApp(space, app)
	defer unlock()

	ctx := r.Context()
	net, err := s.rt.Network(ctx, space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	protectedSomewhere := net.Protected
	for _, pr := range net.Routes {
		protectedSomewhere = protectedSomewhere || pr.Protected
	}
	if !protectedSomewhere {
		writeErr(w, http.StatusConflict, fmt.Errorf("access rules need the login in front of the app — switch it to login-protected first"))
		return
	}

	if err := s.rt.SetAccess(ctx, space, app, access); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	console := s.readConsoleChoices(ctx, space, app)
	console.Access = access
	if err := s.writeConsoleChoices(ctx, space, app, console); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "access": access})
}

func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// writeConsoleChoices stores the console's network choices with the deployed
// app — as app metadata the deploy pipeline never rewrites, so they survive
// deploys untouched.
func (s *Server) writeConsoleChoices(ctx context.Context, space, app string, c consoleChoices) error {
	var value *string
	// Access is a pointer field of a comparable struct, so a non-nil one
	// already makes c differ from the zero value.
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
