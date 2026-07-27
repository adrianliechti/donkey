package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	bkclient "github.com/moby/buildkit/client"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
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
	if app == addons.SharedOwner {
		return "", "", fmt.Errorf("%q is reserved for a space's shared services", addons.SharedOwner)
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
	// Exposure is public, protected or internal — how the app is reached.
	Exposure string `json:"exposure,omitempty"`
	// Port the app listens on (donkey.yaml's `port`).
	Port int `json:"port,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.rt.Status(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
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
		HasStorage: s.rt.HasData(r.Context(), space, app),
	}
	if !st.StartedAt.IsZero() {
		resp.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
	}
	if st.IngressHost != "" {
		resp.URL = s.cfg.Scheme() + "://" + st.IngressHost
	}
	if st.Found {
		resp.Exposure = exposureOf(st)
		resp.Port = int(st.Port)
	}
	writeJSON(w, http.StatusOK, resp)
}

// exposureOf derives the console's exposure word from runtime state.
func exposureOf(st *runtime.AppStatus) string {
	switch {
	case st.IngressHost == "":
		return "internal"
	case st.Protected:
		return "protected"
	default:
		return "public"
	}
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
		Name string   `json:"name"`
		Keys []string `json:"keys"`
	}
	out := []entry{}
	for _, b := range bindings {
		out = append(out, entry{Name: b.Name, Keys: b.Keys})
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
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
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
	// A space's services belong to the pseudo-app "shared", whatever app the
	// caller came through — their pods carry that owner label, not the app's.
	if strings.HasPrefix(component, "addon-") {
		app = addons.SharedOwner
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
	// PORT is donkey's own: it is rendered from donkey.yaml's `port` directly
	// on the container, where it always beats the env secret — accepting a
	// new value here would store something that never reaches the app. An
	// unchanged PORT is tolerated: clients read-modify-write the whole map,
	// and a leftover from before this rule must not brick every other edit.
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
	Type     string `json:"type"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Storage  string `json:"storage"`
	External bool   `json:"external"`
	URL      string `json:"url"`
}

func (r AddonRequest) serviceSpec() runtime.ServiceSpec {
	return runtime.ServiceSpec{
		Type: r.Type, Name: r.Name, Version: r.Version, Storage: r.Storage,
		External: r.External, URL: r.URL,
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

	shared, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	created := false
	var version, storage string
	if len(shared) == 0 {
		if req.Type == "" {
			writeErr(w, http.StatusNotFound, fmt.Errorf("the space has no service named %q — give a type to create it", req.Name))
			return
		}
		shared, version, storage, err = s.rt.EnsureService(ctx, space, req.serviceSpec())
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		created = true
	}

	wired, err := s.rt.AttachService(ctx, space, app, req.Name, shared)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "created": created, "restarted": wired, "version": version, "storage": storage,
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
// type with its defaults, its one-liner, and the env contract connected
// apps will see — the names only, never values.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Type        string   `json:"type"`
		Version     string   `json:"version"`
		Storage     string   `json:"storage"`
		Description string   `json:"description"`
		Keys        []string `json:"keys"`
	}
	out := []entry{}
	for _, a := range addons.Types() {
		// A throwaway binding yields the contract's variable names.
		dummy := addons.Input{
			App: addons.SharedOwner, Space: "space", Namespace: "ns",
			Name: a.Type(), Version: a.DefaultVersion(), Storage: a.DefaultStorage(), Password: "x",
		}
		keys := slices.Sorted(maps.Keys(a.Binding(dummy)))
		out = append(out, entry{
			Type: a.Type(), Version: a.DefaultVersion(), Storage: a.DefaultStorage(),
			Description: a.Description(), Keys: keys,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out})
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
		Name       string   `json:"name"`
		Keys       []string `json:"keys"`
		AttachedTo []string `json:"attachedTo"`
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
		out = append(out, entry{Name: b.Name, Keys: b.Keys, AttachedTo: attached})
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
	_, version, storage, err := s.rt.EnsureService(ctx, space, req.serviceSpec())
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "storage": storage})
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
	data, err := s.rt.ServiceBinding(r.Context(), space, addons.SharedOwner, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if len(data) == 0 {
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
	writeJSON(w, http.StatusOK, map[string]any{"binding": data, "host": host, "port": port})
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
	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 10*time.Second)
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
	detached, err := s.rt.RemoveService(r.Context(), space, name, keepData)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// builderChecks probes what this installation needs in order to build an app
// that brings no Dockerfile. Every image is resolved through the build engine
// rather than from here, so the answer accounts for the engine's mirrors —
// the whole point on a network where donkey-server and the engine reach
// different things.
func (s *Server) builderChecks(ctx context.Context) []DoctorCheck {
	builder := s.cfg.Build.BuilderOrDefault()
	image := func(name, ref string) DoctorCheck {
		c := DoctorCheck{Name: name, Detail: ref}
		if err := build.CheckImage(ctx, s.cfg.BuildkitAddr, s.cfg.Registry, ref, s.cfg.DefaultPlatform); err != nil {
			c.Detail = err.Error()
			return c
		}
		c.OK = true
		return c
	}

	var checks []DoctorCheck
	if builder == config.BuildBuildpacks {
		checks = append(checks, image("Buildpacks builder image", s.cfg.Build.BuildpacksBuilderImage()))
		// An empty run image is the normal case — the lifecycle reads the
		// builder's own. There is nothing to probe until one is named.
		if run := s.cfg.Build.Buildpacks.RunImage; run != "" {
			checks = append(checks, image("Buildpacks run image", run))
		}
	} else {
		builderImage, runtimeImage := build.RailpackImages()
		checks = append(checks,
			image("Railpack builder image", builderImage),
			image("Railpack runtime image", runtimeImage),
		)
	}

	// railpack resolves package versions by running mise, and downloads it
	// from github.com if donkey hasn't staged one first. Checked whichever
	// builder the installation chose: an app's own donkey.yaml can ask for
	// railpack regardless, and this is the check that says whether that
	// would work.
	mise := DoctorCheck{Name: "Railpack version resolver (mise " + build.MiseVersion() + ")"}
	if detail, err := build.CheckMise(ctx); err != nil {
		mise.Detail = err.Error()
	} else {
		mise.OK = true
		mise.Detail = detail
	}
	return append(checks, mise)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	// Longer than the other probes want, because the builder checks pull
	// image manifests through whatever mirrors and proxies stand in the way —
	// which is exactly the setup that is slow when it is misconfigured.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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
