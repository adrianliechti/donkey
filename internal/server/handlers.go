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
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/k8s"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// spaceApp validates the space and app values of a request path and returns
// the space's namespace. They come straight from the URL and feed namespace,
// object and hostname names — so they follow donkey.yaml's naming rules, and
// donkey's own namespace is out of bounds.
func (s *Server) spaceApp(r *http.Request) (ns, app string, err error) {
	app = r.PathValue("app")
	if !config.ValidName(app) {
		return "", "", fmt.Errorf("%q is not a valid app name", app)
	}
	if app == addons.SharedOwner {
		return "", "", fmt.Errorf("%q is reserved for a space's shared services", addons.SharedOwner)
	}
	ns, err = s.cfg.SpaceNamespace(r.PathValue("space"))
	return ns, app, err
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.k8s.AppStatus(r.Context(), ns, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	resp := StatusResponse{
		Found:    st.Found,
		Healthy:  st.Healthy(),
		Ready:    st.ReadyText(),
		Image:    st.Image,
		Pods:     st.Pods,
		Restarts: st.Restarts,
		// The claim is the disk: it outlives the pods, so this is true even
		// while the app is stopped or crash-looping — which is exactly when
		// someone needs to look at what is on it.
		HasStorage: s.k8s.HasDataClaim(r.Context(), ns, app),
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

// exposureOf derives the console's exposure word from cluster state.
func exposureOf(st *k8s.AppStatus) string {
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	restarted, err := s.k8s.RestartApp(r.Context(), ns, app)
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bindings, err := s.k8s.ListAddonBindings(r.Context(), ns, app)
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	ev := s.k8s.RolloutEvidenceFor(ctx, ns, app)
	resp := DiagnoseResponse{Pods: ev.PodSummaries, Events: ev.Events, Logs: ev.PodLogs}
	proposal, aerr := s.assistant.DiagnoseFailure(ctx, &ai.FailureContext{
		Stage:        "rollout",
		Logs:         ev.PodLogs,
		Events:       ev.Events,
		KnownSecrets: s.knownSecrets(ctx, ns, app),
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
	ns, app, err := s.spaceApp(r)
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
	err = s.k8s.StreamLogs(r.Context(), ns, app, k8s.LogOptions{
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	env, err := s.k8s.GetEnv(r.Context(), ns, app)
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
	ns, app, err := s.spaceApp(r)
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
		stored, gerr := s.k8s.GetEnv(r.Context(), ns, app)
		if gerr != nil || stored["PORT"] != v {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("PORT is set by donkey from the `port` in donkey.yaml — change it there and deploy again (a stale stored PORT disappears with `donkey env unset PORT`)"))
			return
		}
	}
	if err := s.k8s.SetEnv(r.Context(), ns, r.PathValue("space"), app, req.Env); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	restarted := false
	if req.Restart {
		var err error
		if restarted, err = s.k8s.RestartApp(r.Context(), ns, app); err != nil {
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

// handleAddAppService connects the app to one of the space's services — services
// belong to the space, apps hold attachments (a copy of the binding Secret,
// wired into their envFrom). A request naming a type creates the service
// first when the space doesn't have it yet.
func (s *Server) handleAddAppService(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	space := r.PathValue("space")
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
	// to take over a namespace donkey didn't create.
	if err := s.k8s.CheckNamespaceFree(ctx, ns); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}

	shared, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(addons.SharedOwner, req.Name))
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
		shared, version, storage, err = s.createSpaceService(ctx, space, ns, req)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		created = true
	}

	if err := s.k8s.Apply(ctx, []runtime.Object{addons.AttachmentSecret(space, app, ns, req.Name, shared)}); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	wired, err := s.k8s.SetAddonEnvFrom(ctx, ns, app, addons.SecretName(app, req.Name), true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "created": created, "restarted": wired, "version": version, "storage": storage,
	})
}

// userError marks a failure the caller caused (a bad type, name or URL), so
// handlers answer 400 instead of blaming the cluster.
type userError struct{ error }

func statusFor(err error) int {
	var ue userError
	if errors.As(err, &ue) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// createSpaceService creates the space's service — provisioned in-cluster, or
// external as a binding-only Secret — and returns its binding values.
func (s *Server) createSpaceService(ctx context.Context, space, ns string, req AddonRequest) (map[string]string, string, string, error) {
	addon, err := addons.Get(req.Type)
	if err != nil {
		return nil, "", "", userError{err}
	}
	if req.External {
		binding, err := addon.ExternalBinding(req.URL)
		if err != nil {
			return nil, "", "", userError{err}
		}
		objs := []runtime.Object{
			manifests.Namespace(space, ns),
			addons.BindingSecret(space, addons.SharedOwner, ns, req.Name, binding),
		}
		if err := s.k8s.Apply(ctx, objs); err != nil {
			return nil, "", "", err
		}
		return binding, "", "", nil
	}
	if !config.ValidName(req.Name) {
		return nil, "", "", userError{fmt.Errorf("%q is not a valid service name — lowercase letters, digits and dashes", req.Name)}
	}
	// The service takes its plain name, reachable as e.g. "postgres" the way
	// siblings reach each other — so it must not seize a name an app (or a
	// differently-named service) already holds.
	if component, exists, err := s.k8s.DeploymentComponent(ctx, ns, req.Name); err != nil {
		return nil, "", "", err
	} else if exists && component != addons.Component(req.Name) {
		return nil, "", "", userError{fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", req.Name)}
	}
	in, err := s.provisionService(ctx, space, ns, addons.SharedOwner, req)
	if err != nil {
		return nil, "", "", err
	}
	return addon.Binding(in), in.Version, in.Storage, nil
}

// provisionService creates (or re-applies) one managed addon instance with its
// binding secret and waits until it serves. Shared by the addon API and the
// deploy path, which provisions what donkey.yaml lists.
func (s *Server) provisionService(ctx context.Context, space, ns, app string, req AddonRequest) (addons.Input, error) {
	addon, err := addons.Get(req.Type)
	if err != nil {
		return addons.Input{}, err
	}
	in := addons.Input{App: app, Space: space, Namespace: ns, Name: req.Name, Version: req.Version, Storage: req.Storage}
	if in.Version == "" {
		in.Version = addon.DefaultVersion()
	}
	if in.Storage == "" {
		in.Storage = addon.DefaultStorage()
	}
	existing, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(app, req.Name))
	if err != nil {
		return in, err
	}
	// Reuse the credential the instance was initialized with — rotating it
	// here would lock the service out of its own kept data.
	if pw, ok := existing[addon.CredentialKey()]; ok {
		in.Password = pw
	} else if in.Password, err = addons.NewPassword(); err != nil {
		return in, err
	}
	objs := []runtime.Object{
		manifests.Namespace(space, ns),
		addons.BindingSecret(space, app, ns, in.Name, addon.Binding(in)),
	}
	addonObjs, err := addon.Objects(in)
	if err != nil {
		return in, err
	}
	objs = append(objs, addonObjs...)
	if err := s.k8s.Apply(ctx, objs); err != nil {
		return in, err
	}
	// A binding-only service (like openai) runs nothing — there is no
	// rollout to wait for.
	if len(addonObjs) == 0 {
		return in, nil
	}
	return in, s.k8s.WaitForRollout(ctx, ns, in.Name)
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
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	// Unwire before deleting: a pod template that still references the
	// binding secret would fail to start once the secret is gone.
	if _, err := s.k8s.SetAddonEnvFrom(r.Context(), ns, app, addons.SecretName(app, name), false); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.k8s.DetachAddon(r.Context(), ns, app, addons.Component(name)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListSpaceServices shows the space's services and who uses them —
// names and injected env var names only, never the values.
func (s *Server) handleListSpaceServices(w http.ResponseWriter, r *http.Request) {
	ns, err := s.cfg.SpaceNamespace(r.PathValue("space"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bindings, err := s.k8s.ListAddonBindings(r.Context(), ns, addons.SharedOwner)
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
		attached, err := s.k8s.ListAddonAttachments(r.Context(), ns, addons.Component(b.Name))
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
	space := r.PathValue("space")
	ns, err := s.cfg.SpaceNamespace(space)
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
	if err := s.k8s.CheckNamespaceFree(ctx, ns); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	shared, version, storage, err := s.createSpaceService(ctx, space, ns, req)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Re-creating an external service may change its address — the copies
	// the attached apps hold have to follow, and their pods restart to see
	// the new values. (A managed re-apply keeps host and credential, so the
	// copies come out identical and nothing restarts.)
	attached, err := s.k8s.ListAddonAttachments(ctx, ns, addons.Component(req.Name))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for _, app := range attached {
		if err := s.k8s.Apply(ctx, []runtime.Object{addons.AttachmentSecret(space, app, ns, req.Name, shared)}); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if req.External {
			if _, err := s.k8s.RestartApp(ctx, ns, app); err != nil {
				writeErr(w, http.StatusBadGateway, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "storage": storage})
}

// handleServiceBinding hands over the binding values plus the service's
// in-cluster address — what `donkey addon tunnel` prints and dials. Values
// go only to the token holder, the same trust level as the app env API.
func (s *Server) handleServiceBinding(w http.ResponseWriter, r *http.Request) {
	ns, err := s.cfg.SpaceNamespace(r.PathValue("space"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	data, err := s.k8s.GetSecretData(r.Context(), ns, addons.SecretName(addons.SharedOwner, name))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusNotFound, fmt.Errorf("the space has no service named %q", name))
		return
	}
	// A missing Service means the addon is external — reachable at its own
	// address, so there is nothing to tunnel to.
	host, port, err := s.k8s.ServiceEndpoint(r.Context(), ns, name)
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
	ns, err := s.cfg.SpaceNamespace(r.PathValue("space"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	host, port, err := s.k8s.ServiceEndpoint(r.Context(), ns, name)
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
	ns, err := s.cfg.SpaceNamespace(r.PathValue("space"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid service name", name))
		return
	}
	ctx := r.Context()
	keepData := r.URL.Query().Get("keepData") == "true"
	attached, err := s.k8s.ListAddonAttachments(ctx, ns, addons.Component(name))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for _, app := range attached {
		if _, err := s.k8s.SetAddonEnvFrom(ctx, ns, app, addons.SecretName(app, name), false); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.k8s.DetachAddon(ctx, ns, app, addons.Component(name)); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}
	if err := s.k8s.DeleteComponent(ctx, ns, addons.SharedOwner, addons.Component(name), keepData); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detached": attached})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	keepData := r.URL.Query().Get("keepData") == "true"
	// A file-browser helper still holding the disk would keep the volume in
	// Terminating — and it is a Pod, which the app's own delete never covers.
	if err := s.k8s.RemoveFilesHelper(r.Context(), ns, app); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.k8s.DeleteAppResources(r.Context(), ns, app, keepData); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The certificate and the gateway grant live next to the gateway, so
	// they need their own goodbye.
	if err := s.detachCertificate(r.Context(), r.PathValue("space"), app); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.k8s.DeleteObject(r.Context(), "ReferenceGrant", manifests.SystemNamespace, manifests.GatewayGrantName(r.PathValue("space"), app)); err != nil {
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

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
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

	// Everything donkey serves hangs on the shared Gateway — an unprogrammed
	// one means Envoy Gateway is missing or unhappy, and nothing has a URL.
	gw := DoctorCheck{Name: "Gateway (Envoy)"}
	if ok, detail, err := s.k8s.GatewayProgrammed(ctx, s.cfg.GatewayNamespace, s.cfg.GatewayName); err != nil {
		gw.Detail = err.Error()
	} else if !ok {
		gw.Detail = "not serving yet: " + detail
	} else {
		gw.OK = true
		gw.Detail = "serving"
	}
	checks = append(checks, gw)

	certs := DoctorCheck{Name: "Certificates (cert-manager)"}
	if err := s.k8s.CertManagerInstalled(ctx); err != nil {
		certs.Detail = err.Error()
	} else if ready, detail, err := s.k8s.IssuerReady(ctx, s.cfg.TLSIssuerKind, s.cfg.TLSIssuer, s.cfg.GatewayNamespace); err != nil {
		certs.Detail = fmt.Sprintf("%s %q: %v", s.cfg.TLSIssuerKind, s.cfg.TLSIssuer, err)
	} else if !ready {
		certs.Detail = fmt.Sprintf("%s %q is not ready: %s", s.cfg.TLSIssuerKind, s.cfg.TLSIssuer, detail)
	} else {
		certs.OK = true
		certs.Detail = fmt.Sprintf("%s %s", s.cfg.TLSIssuerKind, s.cfg.TLSIssuer)
	}
	checks = append(checks, certs)

	// Every app rides through the donkey gateway — if it is down, so is
	// every app, which is exactly what doctor exists to say plainly.
	dg := DoctorCheck{Name: "App gateway (donkey-gateway)"}
	gwURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/healthz",
		manifests.GatewayAppName, manifests.SystemNamespace, manifests.GatewayHealthServicePort)
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, gwURL, nil); err != nil {
		dg.Detail = err.Error()
	} else if res, err := http.DefaultClient.Do(req); err != nil {
		dg.Detail = err.Error()
	} else {
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			dg.OK = true
			dg.Detail = "routing"
		} else {
			dg.Detail = fmt.Sprintf("answered %d", res.StatusCode)
		}
	}
	checks = append(checks, dg)

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

// maxAnalyzeBytes caps what one analyze request may send to the model —
// without it the endpoint is an unbounded proxy onto somebody's AI budget.
const maxAnalyzeBytes = 1 << 20

func (s *Server) handleAIAnalyze(w http.ResponseWriter, r *http.Request) {
	var in ai.AnalyzeInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAnalyzeBytes)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.assistant.Analyze(r.Context(), &in)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
