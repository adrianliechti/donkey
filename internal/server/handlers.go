package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	bkclient "github.com/moby/buildkit/client"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/serviceproxy"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// spaceApp validates the space and app values of a request path. They come
// straight from the URL and feed object and hostname names — so they follow
// donkey.yaml's naming rules, and donkey's own territory is out of bounds.
func (s *Server) spaceApp(r *http.Request) (space, app string, err error) {
	app = r.PathValue("app")
	if !config.ValidName(app) {
		return "", "", fmt.Errorf("%q is not a valid app name", app)
	}
	space = r.PathValue("space")
	if err := s.rt.ValidateSpace(space); err != nil {
		return "", "", err
	}
	return space, app, nil
}

// space validates the space value of a request path.
func (s *Server) space(r *http.Request) (string, error) {
	space := r.PathValue("space")
	if err := s.rt.ValidateSpace(space); err != nil {
		return "", err
	}
	return space, nil
}

type StatusResponse struct {
	Found    bool     `json:"found"`
	Healthy  bool     `json:"healthy"`
	Ready    string   `json:"ready"`
	Image    string   `json:"image"`
	URL      string   `json:"url"`
	Pods     []string `json:"pods"`
	Restarts int32    `json:"restarts,omitempty"`
	// StartedAt is when the newest pod came up (RFC 3339; empty when none).
	StartedAt string `json:"startedAt,omitempty"`
	// HasStorage is true when the app owns a disk. The file browser is only
	// meaningful then — an app with no volume has nothing donkey can show
	// that a redeploy would not rebuild.
	HasStorage bool `json:"hasStorage"`
	// Mode is web or api; exposure is public, protected or internal.
	Mode     string `json:"mode,omitempty"`
	Exposure string `json:"exposure,omitempty"`
	// Port the app listens on (donkey.yaml's `port`).
	Port int `json:"port,omitempty"`
	// Endpoint is the asynchronously prepared edge behind URL. It is omitted
	// for internal apps and runtimes without endpoint diagnostics.
	Endpoint *EndpointStatusResponse `json:"endpoint,omitempty"`
}

type EndpointStatusResponse struct {
	Host        string                    `json:"host"`
	Certificate CertificateStatusResponse `json:"certificate"`
	DNS         DNSStatusResponse         `json:"dns"`
}

type CertificateStatusResponse struct {
	State       string `json:"state"`
	Mode        string `json:"mode"`
	Issuer      string `json:"issuer,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	RenewalTime string `json:"renewalTime,omitempty"`
	Renewing    bool   `json:"renewing,omitempty"`
}

type DNSStatusResponse struct {
	State      string   `json:"state"`
	Observed   bool     `json:"observed"`
	RecordType string   `json:"recordType,omitempty"`
	Expected   []string `json:"expected,omitempty"`
	Message    string   `json:"message,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	role, err := s.spaceRole(r.Context(), space)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if role == "" {
		writeAccessError(w, errSpaceForbidden)
		return
	}
	st, err := s.rt.Status(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	hasStorage := false
	if role != config.SpaceViewer {
		hasStorage = s.rt.HasData(r.Context(), space, app)
	}
	resp := StatusResponse{
		Found:    st.Found,
		Healthy:  st.Healthy(),
		Ready:    st.ReadyText(),
		Image:    st.Image,
		Pods:     st.Instances,
		Restarts: st.Restarts,
		// The claim is the disk: it outlives the pods, so this is true even
		// while the app is stopped or crash-looping — which is exactly when
		// someone needs to look at what is on it.
		HasStorage: hasStorage,
	}
	if !st.StartedAt.IsZero() {
		resp.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
	}
	if st.PublicHost != "" {
		resp.URL = s.cfg.Scheme() + "://" + st.PublicHost
		if provider, ok := s.rt.(runtime.EndpointStatusProvider); ok && role != config.SpaceViewer {
			resp.Endpoint = endpointStatusResponse(st.PublicHost, provider.EndpointStatus(
				r.Context(), space, app, st.PublicHost,
			))
		}
	}
	if st.Found {
		resp.Exposure = exposureOf(st)
		resp.Mode = modeOfStatus(st)
		resp.Port = int(st.Port)
	}
	if role == config.SpaceViewer {
		resp.Image, resp.Pods, resp.Endpoint = "", nil, nil
		resp.Port, resp.HasStorage = 0, false
	}
	writeJSON(w, http.StatusOK, resp)
}

func endpointStatusResponse(host string, status runtime.EndpointStatus) *EndpointStatusResponse {
	certificate := status.Certificate
	dns := status.DNS
	resp := &EndpointStatusResponse{
		Host: host,
		Certificate: CertificateStatusResponse{
			State:    string(certificate.State),
			Mode:     certificate.Mode,
			Issuer:   certificate.Issuer,
			Reason:   certificate.Reason,
			Message:  certificate.Message,
			Renewing: certificate.Renewing,
		},
		DNS: DNSStatusResponse{
			State:      string(dns.State),
			Observed:   dns.Observed,
			RecordType: dns.RecordType,
			Expected:   dns.Expected,
			Message:    dns.Message,
		},
	}
	if !certificate.ExpiresAt.IsZero() {
		resp.Certificate.ExpiresAt = certificate.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if !certificate.RenewalTime.IsZero() {
		resp.Certificate.RenewalTime = certificate.RenewalTime.UTC().Format(time.RFC3339)
	}
	return resp
}

// exposureOf derives the console's exposure word from runtime state.
func exposureOf(st *runtime.AppStatus) string {
	switch {
	case st.PublicHost == "":
		return "internal"
	case st.Protected:
		return "protected"
	default:
		return "public"
	}
}

func modeOfStatus(st *runtime.AppStatus) string {
	if st.Mode != "" {
		return st.Mode
	}
	return config.AppModeWeb
}

// handleRestart rolls the app's pods without changing anything else — the
// safe restart the console offers next to env editing.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	restarted, err := s.rt.Restart(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !restarted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("nothing with this name is deployed — nothing to restart"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListAppServices shows which services are bound to the app — names and
// injected env var names only, never the values.
func (s *Server) handleListAppServices(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bindings, err := s.rt.ListServices(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type entry struct {
		Name       string         `json:"name"`
		Keys       []string       `json:"keys"`
		Type       string         `json:"type,omitempty"`
		Protocol   string         `json:"protocol,omitempty"`
		Parameters map[string]any `json:"parameters,omitempty"`
	}
	out := []entry{}
	for _, b := range bindings {
		out = append(out, entry{Name: b.Name, Keys: b.Keys, Type: b.Type, Protocol: b.Protocol, Parameters: b.Parameters})
	}
	writeJSON(w, http.StatusOK, map[string]any{"addons": out})
}

// DiagnoseResponse is what the console's "explain what's wrong" shows: the
// raw evidence always, the assistant's reading of it when it answers.
type DiagnoseResponse struct {
	Diagnosis      string         `json:"diagnosis,omitempty"`
	Proposals      []ai.FixAction `json:"proposals,omitempty"`
	Pods           []string       `json:"pods,omitempty"`
	Events         []string       `json:"events,omitempty"`
	Logs           string         `json:"logs,omitempty"`
	AssistantError string         `json:"assistantError,omitempty"`
}

// handleDiagnose gathers the same evidence a failed rollout collects and
// asks the assistant to explain it — on demand, for an app that is red now.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ev := s.rt.Evidence(ctx, space, app)
	resp := DiagnoseResponse{Pods: ev.Summaries, Events: ev.Events, Logs: ev.Logs}
	proposal, aerr := s.assistant.DiagnoseFailure(ctx, &ai.FailureContext{
		Stage:        "rollout",
		Logs:         ev.Logs,
		Events:       ev.Events,
		KnownSecrets: s.knownSecrets(ctx, space, app),
	})
	switch {
	case aerr != nil:
		resp.AssistantError = aerr.Error()
	case proposal != nil:
		resp.Diagnosis = proposal.Diagnosis
		resp.Proposals = proposal.Actions
	}
	writeJSON(w, http.StatusOK, resp)
}

// validComponent bounds what may reach a provider as part of an identifier.
// On Docker the component becomes a container name, which the engine client
// path.Joins into its request URL — so an unchecked "addon-x/../../.." would
// collapse out of the containers endpoint and read any container on the host.
func validComponent(c string) bool {
	if c == "web" {
		return true
	}
	name, ok := strings.CutPrefix(c, "addon-")
	return ok && config.ValidName(name)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	// An explicit 0 means "no history, just what comes next" (with follow);
	// only a missing or unusable value falls back to the default.
	tail, perr := strconv.ParseInt(q.Get("tail"), 10, 64)
	if q.Get("tail") == "" || perr != nil || tail < 0 {
		tail = 200
	}
	var since time.Duration
	if v := q.Get("since"); v != "" {
		since, _ = time.ParseDuration(v)
	}
	component := q.Get("component")
	if component == "" {
		component = "web"
	}
	// The component reaches a provider as part of an identifier — a label
	// selector on Kubernetes, a container name on Docker — so it is validated
	// here, once, for every runtime. Only "web" and "addon-<name>" exist.
	if !validComponent(component) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a part of this app — use web, or addon-<service>", component))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	err = s.rt.Logs(r.Context(), space, app, runtime.LogOptions{
		Component: component,
		Follow:    q.Get("follow") == "true",
		TailLines: tail,
		Since:     since,
	}, flushWriter{w})
	if err != nil && r.Context().Err() == nil {
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	env, err := s.rt.GetEnv(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": env})
}

type PutEnvRequest struct {
	Env     map[string]string `json:"env"`
	Restart bool              `json:"restart"`
}

func (s *Server) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req PutEnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// PORT comes from donkey.yaml's `port` directly on the container, where it
	// beats the env secret — accepting one here would store what never reaches
	// the app. An unchanged PORT is tolerated, since clients read-modify-write
	// the whole map.
	if v, ok := req.Env["PORT"]; ok {
		stored, gerr := s.rt.GetEnv(r.Context(), space, app)
		if gerr != nil || stored["PORT"] != v {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("PORT is set by donkey from the `port` in donkey.yaml — change it there and deploy again (a stale stored PORT disappears with `donkey env unset PORT`)"))
			return
		}
	}
	if err := s.rt.SetEnv(r.Context(), space, app, req.Env); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	restarted := false
	if req.Restart {
		var err error
		if restarted, err = s.rt.Restart(r.Context(), space, app); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(req.Env), "restarted": restarted})
}

type AddonRequest struct {
	// Type is empty when the request attaches a service the space already
	// has; naming one creates the service first if it is missing.
	Type string `json:"type"`
	Name string `json:"name"`
	// Parameters are the type's own settings, as the catalog's schema
	// describes them. Version and Storage are shorthand for the parameters
	// of those names.
	Parameters map[string]any `json:"parameters,omitempty"`
	Version    string         `json:"version,omitempty"`
	Storage    string         `json:"storage,omitempty"`
	External   bool           `json:"external"`
	URL        string         `json:"url"`
}

func (r AddonRequest) serviceSpec() runtime.ServiceSpec {
	return runtime.ServiceSpec{
		Type: r.Type, Name: r.Name,
		Parameters: config.AddonConfig{Version: r.Version, Storage: r.Storage, Parameters: r.Parameters}.ServiceParameters(),
		External:   r.External, URL: r.URL,
	}
}

// handleAddAppService connects the app to one of the space's services — services
// belong to the space, apps hold attachments (a copy of the binding Secret,
// wired into their envFrom). A request naming a type creates the service
// first when the space doesn't have it yet.
func (s *Server) handleAddAppService(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req AddonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		req.Name = req.Type
	}
	if !config.ValidName(req.Name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name — lowercase letters, digits and dashes", req.Name))
		return
	}
	ctx := r.Context()
	// The addon may be the first thing in a fresh space — but never a reason
	// to take over territory donkey didn't create.
	if err := s.rt.CheckSpaceFree(ctx, space); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err := runtime.CheckServiceAttachment(ctx, s.rt, s.cfg.CommonOfferings, space, app, req.Name); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}

	connection, err := s.rt.ServiceConnection(ctx, space, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if req.Type != "" {
		if err := (addons.Metadata{Type: connection.Type, Protocol: connection.Protocol}).CheckType(req.Type); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	shared := connection.Binding
	created := false
	var parameters map[string]any
	if len(shared) == 0 || req.External {
		if req.Type == "" {
			writeErr(w, http.StatusNotFound, fmt.Errorf("the space has no service named %q — give a type to create it", req.Name))
			return
		}
		shared, parameters, err = s.rt.EnsureService(ctx, space, req.serviceSpec())
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		created = len(connection.Binding) == 0
	}

	wired, err := s.rt.AttachService(ctx, space, app, req.Name, shared)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "created": created, "restarted": wired, "parameters": parameters,
	})
}

func statusFor(err error) int {
	var ue runtime.UserError
	if errors.As(err, &ue) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// handleCatalog is the service catalog: what the console can offer, each
// entry with its descriptor — title, category, protocol, the JSON Schema of
// its parameters and the env contract connected apps will see (names only,
// never values). A provider that has never answered is listed once, under
// its name and marked unavailable, so the console can say so instead of
// silently losing it; a provider with a cached catalog never delays this.
// Deprecated offerings are left out: they serve existing instances only.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	space := r.PathValue("space")
	var policy config.SpaceConfig
	if space != "" {
		var err error
		policy, err = runtime.OfferingPolicy(r.Context(), s.rt, space)
		if err != nil {
			writeAccessError(w, err)
			return
		}
	}
	type envEntry struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Secret      bool   `json:"secret,omitempty"`
		Optional    bool   `json:"optional,omitempty"`
	}
	type capabilities struct {
		RetainData bool `json:"retainData"`
		Update     bool `json:"update"`
	}
	type entry struct {
		Type               string         `json:"type"`
		Provider           string         `json:"provider,omitempty"`
		Protocol           string         `json:"protocol,omitempty"`
		Title              string         `json:"title"`
		Description        string         `json:"description"`
		Category           string         `json:"category,omitempty"`
		Tags               []string       `json:"tags,omitempty"`
		Icon               string         `json:"icon,omitempty"`
		DocumentationURL   string         `json:"documentationUrl,omitempty"`
		Shared             bool           `json:"shared,omitempty"`
		Parameters         map[string]any `json:"parameters,omitempty"`
		SpaceConfiguration map[string]any `json:"spaceConfiguration,omitempty"`
		Common             bool           `json:"common"`
		Env                []envEntry     `json:"env"`
		Keys               []string       `json:"keys"`
		Capabilities       capabilities   `json:"capabilities"`
		Available          bool           `json:"available"`
		Error              string         `json:"error,omitempty"`
	}
	out := []entry{}
	for _, a := range addons.Types(r.Context()) {
		if space != "" && !policy.OfferingAllowed(s.cfg.CommonOfferings, a.Type()) {
			continue
		}
		provider := ""
		if p, ok := a.(*addons.Plugin); ok {
			provider = p.Provider()
		}
		desc, err := a.Describe(r.Context())
		if err != nil {
			out = append(out, entry{Type: a.Type(), Provider: provider, Title: a.Type(), Env: []envEntry{}, Keys: []string{}, Error: err.Error()})
			continue
		}
		if desc.Deprecated {
			continue
		}
		spaceOffering, _ := policy.Offering(desc.Id)
		e := entry{
			Type: desc.Id, Provider: provider, Protocol: addons.NormalizeProtocol(desc.Protocol), Title: desc.Title, Description: desc.Description,
			Category: desc.Category, Tags: desc.Tags, Icon: desc.Icon, DocumentationURL: desc.DocumentationUrl, Shared: desc.Shared,
			Capabilities: capabilities{RetainData: desc.GetCapabilities().GetRetainData(), Update: desc.GetCapabilities().GetUpdate()},
			Available:    true, Env: []envEntry{}, Keys: addons.EnvNames(desc),
			Common: config.SpaceConfig{}.OfferingAllowed(s.cfg.CommonOfferings, desc.Id),
		}
		if desc.SpaceConfiguration != nil {
			e.SpaceConfiguration = desc.SpaceConfiguration.AsMap()
		}
		if desc.Parameters != nil {
			e.Parameters = desc.Parameters.AsMap()
			// Apply space defaults to the form without changing the provider's
			// descriptor or exposing its admin-managed configuration values.
			if space != "" {
				if properties, ok := e.Parameters["properties"].(map[string]any); ok {
					for key, value := range spaceOffering.Defaults {
						if prop, ok := properties[key].(map[string]any); ok {
							prop["default"] = value
						}
					}
				}
			}
		}
		if space != "" {
			if _, err := addons.ResolveParameters(desc.SpaceConfiguration, spaceOffering.Config); err != nil {
				e.Available, e.Error = false, "Needs configuration from a space admin"
			}
		}
		for _, v := range desc.Env {
			e.Env = append(e.Env, envEntry{Name: v.Name, Description: v.Description, Secret: v.Secret, Optional: v.Optional})
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out, "commonOfferings": s.cfg.CommonOfferings})
}

// handleRemoveAppService detaches the app from a space service — its binding copy
// and env go away, the service (and every byte of its data) stays. Deleting
// the service itself is a space-level operation.
func (s *Server) handleRemoveAppService(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	if err := s.rt.DetachService(r.Context(), space, app, name); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListSpaceServices shows the space's services and who uses them —
// names and injected env var names only, never the values.
func (s *Server) handleListSpaceServices(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bindings, err := s.rt.ListServices(r.Context(), space, addons.SharedOwner)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type entry struct {
		Name       string         `json:"name"`
		Keys       []string       `json:"keys"`
		AttachedTo []string       `json:"attachedTo"`
		Type       string         `json:"type,omitempty"`
		Protocol   string         `json:"protocol,omitempty"`
		Parameters map[string]any `json:"parameters,omitempty"`
		// State is set only for a plugin instance without a binding:
		// pending, failed (with Message) or stopped.
		State   string `json:"state,omitempty"`
		Message string `json:"message,omitempty"`
	}
	out := []entry{}
	for _, b := range bindings {
		attached, err := s.rt.ServiceAttachments(r.Context(), space, b.Name)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if attached == nil {
			attached = []string{}
		}
		out = append(out, entry{Name: b.Name, Keys: b.Keys, Type: b.Type, Protocol: b.Protocol, AttachedTo: attached,
			Parameters: b.Parameters, State: b.State, Message: b.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{"addons": out})
}

// handleAddSpaceService creates (or re-applies) a service of the space without
// attaching any app — the space card's "add".
func (s *Server) handleAddSpaceService(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req AddonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		req.Name = req.Type
	}
	if !config.ValidName(req.Name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name — lowercase letters, digits and dashes", req.Name))
		return
	}
	ctx := r.Context()
	if err := s.rt.CheckSpaceFree(ctx, space); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	_, parameters, err := s.rt.EnsureService(ctx, space, req.serviceSpec())
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "parameters": parameters})
}

// handleServiceBinding hands over the binding values plus the service's
// in-runtime address — what `donkey addon tunnel` prints and dials. Values
// go only to the token holder, the same trust level as the app env API.
func (s *Server) handleServiceBinding(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	connection, err := s.rt.ServiceConnection(r.Context(), space, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if len(connection.Binding) == 0 {
		writeErr(w, http.StatusNotFound, fmt.Errorf("the space has no service named %q", name))
		return
	}
	// A missing endpoint means the addon is external — reachable at its own
	// address, so there is nothing to tunnel to.
	host, port, err := s.rt.ServiceEndpoint(r.Context(), space, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": connection.Binding, "host": host, "port": port, "type": connection.Type, "protocol": connection.Protocol,
		"tunnel": addons.TunnelFor(connection.Type, connection.Protocol),
	})
}

// tunnelStrippedHeaders are donkey's own, like every identity header
// (identity.StripHeaders): an HTTP tunnel never hands them on.
var tunnelStrippedHeaders = []string{"Authorization", "Cookie", "Forwarded", "X-Access-Token",
	addons.TunnelAuthorizationHeader, serviceproxy.TestRequestHeader}

// handleServiceHTTPTunnel carries an HTTP tunnel (`donkey addon tunnel`) to
// the service's URL, shaped by its protocol's addons.Tunnel: what the service
// needs to know who is asking (the caller's IdP token, the space's key, a
// proxy grant), only the paths it allows, and none of donkey's credentials or
// the client headers it must not see. The local client's own Authorization
// passes where donkey adds none.
func (s *Server) handleServiceHTTPTunnel(w http.ResponseWriter, r *http.Request) {
	connection, err := s.serviceConnection(r.Context(), r, addons.HTTPTunnelProtocols()...)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	name := r.PathValue("service")
	tunnel := addons.TunnelFor(connection.Type, connection.Protocol)
	path := "/" + r.PathValue("path")
	if !tunnel.Allows(path) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("the %s tunnel does not carry %s", connection.Protocol, path))
		return
	}
	added := http.Header{}
	if auth := r.Header.Get(addons.TunnelAuthorizationHeader); auth != "" {
		added.Set("Authorization", auth)
	}
	if tunnel.UserToken {
		token, err := s.userToken(r.Context())
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		added.Set("Authorization", "Bearer "+token)
	}
	header, value, ok := tunnel.CredentialHeader(connection.Binding)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%s was attached from elsewhere — reach it at its own address, no tunnel needed", name))
		return
	}
	if header != "" {
		added.Set(header, value)
	}
	base, err := url.Parse(connection.Binding[tunnel.URLKey])
	if tunnel.ProxyGrant {
		base, err = s.proxyBase(connection)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// JoinPath on an empty path yields a relative one ("v1/statement"),
	// which the proxy would send as is in the request line.
	if base.Path == "" {
		base.Path = "/"
	}
	target := base.JoinPath(path)
	target.RawQuery = r.URL.RawQuery
	if tunnel.ProxyGrant {
		space, _ := s.space(r)
		grant, err := serviceproxy.SealTestRequest(s.cfg.TelemetryKey, space, connection.Type, &http.Request{Method: r.Method, URL: target})
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
		added.Set(serviceproxy.TestRequestHeader, grant)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL, pr.Out.Host = target, ""
			identity.StripHeaders(pr.Out.Header)
			for _, h := range append(tunnelStrippedHeaders, tunnel.DropHeaders...) {
				pr.Out.Header.Del(h)
			}
			for h, values := range added {
				pr.Out.Header[h] = values
			}
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			return nil
		},
		// Streams (OpenAI's server-sent events) arrive as they are written.
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

var tunnelUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
}

// handleServiceTunnel carries one TCP connection to a space service over a
// WebSocket — the CLI's private line to a database that rightly has no
// public address. Auth is the bearer token, like every other route.
func (s *Server) handleServiceTunnel(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	host, port, err := s.rt.ServiceEndpoint(r.Context(), space, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if host == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no service named %q runs in this space — one attached from elsewhere is reached at its own address", name))
		return
	}
	// Dial before upgrading, so a dead backend still gets a proper HTTP error.
	backend, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.Context(), "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("%s is not answering: %w", name, err))
		return
	}
	ws, err := tunnelUpgrader.Upgrade(w, r, nil)
	if err != nil {
		backend.Close()
		return
	}
	defer ws.Close()
	defer backend.Close()
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if _, err := backend.Write(msg); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := backend.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
}

// handleRemoveSpaceService removes a service from the space, detaching every
// app that uses it first (their pods restart without the binding).
func (s *Server) handleRemoveSpaceService(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	keepData := r.URL.Query().Get("keepData") == "true"
	// orphan drops a plugin instance whose plugin is gone or failing; its
	// backing service is then the operator's to clean up.
	orphan := r.URL.Query().Get("orphan") == "true"
	detached, err := s.rt.RemoveService(r.Context(), space, name, keepData, orphan)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detached": detached})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	keepData := r.URL.Query().Get("keepData") == "true"
	if err := s.rt.Destroy(r.Context(), space, app, keepData); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The app's build and source history go with it, so a later app by the
	// same name starts with its own. Keeping the data is the redeploy case:
	// the same app comes back to its disk and its history alike. The runtime
	// goes first, so retrying after a telemetry hiccup finishes the job.
	if !keepData {
		if err := s.telemetry.PurgeBuilds(r.Context(), space, app); err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("the app is removed, but its build history is not: %w — removing it again finishes the job", err))
			return
		}
		if s.history != nil {
			if err := s.history.RemoveApp(space, app); err != nil {
				writeErr(w, http.StatusBadGateway, fmt.Errorf("the app is removed, but its source history is not: %w — removing it again finishes the job", err))
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// builderDetail says what donkey's own builder will do, in one line.
func builderDetail(b config.BuildSettings) string {
	if !b.VerifyStartup() {
		return "the assistant writes each app's Dockerfile (startup verification off)"
	}
	return fmt.Sprintf("the assistant writes each app's Dockerfile; images are run and must start within %ds before they are published",
		b.VerifyWindow())
}

// builderChecks has no builder image to probe, which is the design: the
// assistant's Dockerfile names its own bases, and only the finished file can
// ask whether they resolve. That the assistant answers at all is covered by
// the assistant check.
func (s *Server) builderChecks(context.Context) []DoctorCheck {
	return []DoctorCheck{{
		Name: "Builder (donkey writes the Dockerfile)", OK: true,
		Detail: builderDetail(s.cfg.Build),
	}}
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	// Longer than the other probes want, because the builder checks pull
	// image manifests through whatever mirrors and proxies stand in the way —
	// which is exactly the setup that is slow when it is misconfigured.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var checks []DoctorCheck

	bk := DoctorCheck{Name: "Build engine (in-cluster BuildKit)"}
	if c, err := bkclient.New(ctx, s.cfg.BuildkitAddr); err == nil {
		if _, err = c.Info(ctx); err == nil {
			bk.OK = true
		} else {
			bk.Detail = err.Error()
		}
		c.Close()
	} else {
		bk.Detail = err.Error()
	}
	checks = append(checks, bk)

	reg := DoctorCheck{Name: "Registry login"}
	if err := build.CheckRegistryAuth(ctx, s.cfg.Registry); err == nil {
		reg.OK = true
		reg.Detail = s.cfg.Registry.Host()
	} else {
		reg.Detail = err.Error()
	}
	checks = append(checks, reg)

	checks = append(checks, s.builderChecks(ctx)...)

	// The runtime probes its own dependencies — gateway, certificates,
	// whatever this runtime needs to serve apps.
	for _, c := range s.rt.Checks(ctx) {
		checks = append(checks, DoctorCheck{Name: c.Name, OK: c.OK, Detail: c.Detail})
	}

	telemetry := DoctorCheck{Name: "Telemetry (OTLP + DuckDB)"}
	if err := s.telemetry.Ready(ctx); err == nil {
		telemetry.OK = true
		telemetry.Detail = "30-day traffic store answering"
	} else {
		telemetry.Detail = err.Error()
	}
	checks = append(checks, telemetry)

	// donkey needs the assistant to read apps, so doctor checks it like any
	// other dependency: does the configured model actually answer?
	assistant := DoctorCheck{Name: "Assistant (" + s.cfg.AIModel + ")"}
	if err := s.assistant.Check(ctx); err == nil {
		assistant.OK = true
		assistant.Detail = "answering"
	} else {
		assistant.Detail = err.Error()
	}
	checks = append(checks, assistant)

	writeJSON(w, http.StatusOK, map[string]any{"checks": checks, "domain": s.cfg.Domain})
}
