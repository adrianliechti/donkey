package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"k8s.io/apimachinery/pkg/runtime"

	"monobox.ch/donkey/internal/addons"
	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/build"
	"monobox.ch/donkey/internal/k8s"
	"monobox.ch/donkey/internal/k8s/manifests"
)

type StatusResponse struct {
	Found   bool     `json:"found"`
	Healthy bool     `json:"healthy"`
	Ready   string   `json:"ready"`
	Image   string   `json:"image"`
	URL     string   `json:"url"`
	Pods    []string `json:"pods"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	st, err := s.k8s.AppStatus(r.Context(), s.cfg.Namespace(app), app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	resp := StatusResponse{
		Found:   st.Found,
		Healthy: st.Healthy(),
		Ready:   st.ReadyText(),
		Image:   st.Image,
		Pods:    st.Pods,
	}
	if st.IngressHost != "" {
		resp.URL = s.cfg.Scheme() + "://" + st.IngressHost
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	q := r.URL.Query()
	tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64)
	if tail <= 0 {
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	err := s.k8s.StreamLogs(r.Context(), s.cfg.Namespace(app), app, k8s.LogOptions{
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
	app := r.PathValue("app")
	env, err := s.k8s.GetEnv(r.Context(), s.cfg.Namespace(app), app)
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
	app := r.PathValue("app")
	var req PutEnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ns := s.cfg.Namespace(app)
	if err := s.k8s.SetEnv(r.Context(), ns, app, req.Env); err != nil {
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
	Type     string `json:"type"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Storage  string `json:"storage"`
	External bool   `json:"external"`
	URL      string `json:"url"`
	// Image is the app's current image; when set, the server re-applies the
	// app so envFrom picks up the binding.
	Rewire bool `json:"rewire"`
}

func (s *Server) handleAddAddon(w http.ResponseWriter, r *http.Request) {
	appName := r.PathValue("app")
	var req AddonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		req.Name = req.Type
	}
	ns := s.cfg.Namespace(appName)
	ctx := r.Context()

	var objs []runtime.Object
	objs = append(objs, manifests.Namespace(appName, ns))

	if req.External {
		if req.Type != "postgres" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("attach currently supports postgres"))
			return
		}
		binding, err := addons.ExternalPostgresBinding(req.URL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		objs = append(objs, addons.BindingSecret(appName, ns, req.Name, binding))
		if err := s.k8s.Apply(ctx, objs); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	addon, err := addons.Get(req.Type)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	in := addons.Input{AppName: appName, Namespace: ns, Name: req.Name, Version: req.Version, Storage: req.Storage}
	if in.Version == "" {
		in.Version = addon.DefaultVersion()
	}
	if in.Storage == "" {
		in.Storage = addon.DefaultStorage()
	}
	existing, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(appName, req.Name))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if pw, ok := existing["PGPASSWORD"]; ok {
		in.Password = pw
	} else if in.Password, err = addons.NewPassword(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	objs = append(objs, addons.BindingSecret(appName, ns, req.Name, addon.Binding(in)))
	addonObjs, err := addon.Objects(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	objs = append(objs, addonObjs...)
	if err := s.k8s.Apply(ctx, objs); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.k8s.WaitForRollout(ctx, ns, addons.InstanceName(appName, req.Name)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": in.Version, "storage": in.Storage})
}

func (s *Server) handleRemoveAddon(w http.ResponseWriter, r *http.Request) {
	appName := r.PathValue("app")
	name := r.PathValue("addon")
	keepData := r.URL.Query().Get("keepData") == "true"
	if err := s.k8s.DeleteComponent(r.Context(), s.cfg.Namespace(appName), appName, addons.Component(name), keepData); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	appName := r.PathValue("app")
	keepData := r.URL.Query().Get("keepData") == "true"
	if err := s.k8s.DeleteAppResources(r.Context(), s.cfg.Namespace(appName), appName, keepData); err != nil {
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

	aiCheck := DoctorCheck{Name: "AI assistance", OK: true}
	if s.assistant.Enabled() {
		aiCheck.Detail = "active (" + s.cfg.AIModel + ")"
	} else {
		aiCheck.Detail = "not configured"
	}
	checks = append(checks, aiCheck)

	writeJSON(w, http.StatusOK, map[string]any{"checks": checks, "baseDomain": s.cfg.BaseDomain})
}

func (s *Server) handleAISettings(w http.ResponseWriter, r *http.Request) {
	if !s.assistant.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("AI assistance is not configured on the server"))
		return
	}
	var sum ai.RepoSummary
	if err := json.NewDecoder(r.Body).Decode(&sum); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.assistant.SuggestAppSettings(r.Context(), &sum)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAIBuild(w http.ResponseWriter, r *http.Request) {
	if !s.assistant.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("AI assistance is not configured on the server"))
		return
	}
	var sum ai.RepoSummary
	if err := json.NewDecoder(r.Body).Decode(&sum); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.assistant.SuggestBuildConfig(r.Context(), &sum)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
