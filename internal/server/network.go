package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// RouteEntry is one extra address a request can reach the app on.
type RouteEntry struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

// NetworkResponse is what the console's network settings show: read from the
// cluster's objects, never from stored intent.
type NetworkResponse struct {
	// Exposure is public, protected or internal.
	Exposure string `json:"exposure"`
	URL      string `json:"url,omitempty"`
	// DefaultHost is what the app is called without a custom domain.
	DefaultHost string `json:"defaultHost"`
	// Domain is set when the app serves on a custom hostname.
	Domain string       `json:"domain,omitempty"`
	Port   int          `json:"port,omitempty"`
	Routes []RouteEntry `json:"routes,omitempty"`
	// LoginAvailable says the installation has an IdP — without one there is
	// no "protected", only public and internal.
	LoginAvailable bool                 `json:"loginAvailable"`
	Access         *config.AccessConfig `json:"access,omitempty"`
}

func (s *Server) networkResponse(r *http.Request, ns, space, app string) (*NetworkResponse, error) {
	st, err := s.k8s.AppStatus(r.Context(), ns, app)
	if err != nil {
		return nil, err
	}
	if !st.Found {
		return nil, nil
	}
	net, err := s.k8s.AppNetwork(r.Context(), ns, app)
	if err != nil {
		return nil, err
	}
	resp := &NetworkResponse{
		DefaultHost:    app + "." + space + "." + s.cfg.Domain,
		Port:           int(st.Port),
		LoginAvailable: s.cfg.OIDCIssuer != "",
		Access:         net.Access,
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
		if net.Host != resp.DefaultHost {
			resp.Domain = net.Host
		}
	}
	for _, pr := range net.Routes {
		resp.Routes = append(resp.Routes, RouteEntry{Host: pr.Host, Path: pr.Path})
	}
	return resp, nil
}

func (s *Server) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp, err := s.networkResponse(r, ns, r.PathValue("space"), app)
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
	Exposure string `json:"exposure"`
	Domain   string `json:"domain"`
}

// handlePutNetwork changes how an app is reached — without a deploy. The
// route, certificate and gateway grant are re-rendered from cluster state;
// the choice is recorded in the console annotation so later deploys keep it
// (unless donkey.yaml itself says otherwise, which always wins).
func (s *Server) handlePutNetwork(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	space := r.PathValue("space")
	var req PutNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !slices.Contains([]string{"public", "protected", "internal"}, req.Exposure) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("exposure must be public, protected or internal"))
		return
	}
	if req.Exposure == "protected" && s.cfg.OIDCIssuer == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("this installation has no identity provider, so there is no login to put in front of the app — `donkey server install` with auth.issuer adds one"))
		return
	}

	unlock := s.lockApp(ns, app)
	defer unlock()

	ctx := r.Context()
	st, err := s.k8s.AppStatus(ctx, ns, app)
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
	net, err := s.k8s.AppNetwork(ctx, ns, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	console := s.readConsoleChoices(ctx, ns, app)

	// A synthetic app config carries the choice through the same validation
	// and rendering a donkey.yaml would.
	cfg := &config.AppConfig{Name: app, Space: space, Port: int(st.Port)}
	switch req.Exposure {
	case "public":
		cfg.Public = true
	case "internal":
		cfg.Internal = true
	}
	cfg.Domain = strings.TrimSpace(req.Domain)
	if req.Exposure == "protected" {
		// Access rules ride along unchanged; cluster state is the truth,
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

	in, err := s.manifestInput(cfg, st.Image, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// Re-render: the grant whenever any route rides the gateway, the app's
	// own certificate and route unless internal, and every existing path
	// route under its existing name (its exposure follows the app's).
	var objs []runtime.Object
	if in.Gateway != nil && (in.Domain != "" || len(net.Routes) > 0) {
		objs = append(objs, manifests.GatewayGrant(in))
	}
	if in.Domain != "" {
		if in.TLS.Issuer != "" {
			objs = append(objs, manifests.AppCertificateObject(in))
		}
		objs = append(objs, manifests.AppRoute(in))
	}
	for _, pr := range net.Routes {
		objs = append(objs, manifests.PathRouteNamed(in, pr.Name, manifests.RouteTarget{Host: pr.Host, Path: pr.Path}))
	}
	if err := s.k8s.Apply(ctx, objs); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.retractLegacyLogin(ctx, ns, app); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	note := ""
	if in.Domain == "" {
		if err := s.k8s.DeleteObject(ctx, "HTTPRoute", ns, app); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.detachCertificate(ctx, space, app); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if len(net.Routes) == 0 {
			if err := s.k8s.DeleteObject(ctx, "ReferenceGrant", manifests.SystemNamespace, manifests.GatewayGrantName(space, app)); err != nil {
				writeErr(w, http.StatusBadGateway, err)
				return
			}
		}
	} else {
		if err := s.attachCertificate(ctx, space, app); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		note = s.waitForCertificate(ctx, space, app, in.Domain)
		if err := s.waitForRouteReady(ctx, ns, app); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}

	// Record the choice so deploys keep it while donkey.yaml stays silent.
	console.Exposure = req.Exposure
	console.Domain = cfg.Domain
	if err := s.writeConsoleChoices(ctx, ns, app, console); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	resp, err := s.networkResponse(r, ns, space, app)
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
// It is an annotation change on the app's routes — the gateway picks it up
// within seconds, no deploy, no restart.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.cfg.OIDCIssuer == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("this installation has no identity provider, so there are no signed-in users to tell apart"))
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

	unlock := s.lockApp(ns, app)
	defer unlock()

	ctx := r.Context()
	net, err := s.k8s.AppNetwork(ctx, ns, app)
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

	var value *string
	if access != nil {
		b, err := json.Marshal(access)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		v := string(b)
		value = &v
	}
	patch := map[string]*string{manifests.AnnotationAccess: value}
	if net.Protected {
		if err := s.k8s.SetAnnotations(ctx, "HTTPRoute", ns, app, patch); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}
	for _, pr := range net.Routes {
		if !pr.Protected {
			continue
		}
		if err := s.k8s.SetAnnotations(ctx, "HTTPRoute", ns, pr.Name, patch); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}

	console := s.readConsoleChoices(ctx, ns, app)
	console.Access = access
	if err := s.writeConsoleChoices(ctx, ns, app, console); err != nil {
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

// writeConsoleChoices stores the console's network choices on the Deployment
// — as a patch, so the deploy pipeline's server-side apply (which owns only
// its own annotation keys) never wipes them.
func (s *Server) writeConsoleChoices(ctx context.Context, ns, app string, c consoleChoices) error {
	var value *string
	if c != (consoleChoices{}) || c.Access != nil {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		v := string(b)
		value = &v
	}
	return s.k8s.SetAnnotations(ctx, "Deployment", ns, app, map[string]*string{manifests.AnnotationConsole: value})
}
