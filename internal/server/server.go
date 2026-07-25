package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/k8s"
)

type Server struct {
	cfg       *Config
	k8s       *k8s.Client
	assistant ai.Assistant
}

func New(cfg *Config) (*Server, error) {
	client, err := k8s.NewInCluster()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:       cfg,
		k8s:       client,
		assistant: ai.New(os.Getenv("OPENAI_API_KEY") != "", cfg.AIModel),
	}, nil
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("POST /v1/deploys", s.auth(s.handleDeploy))
	mux.Handle("GET /v1/apps", s.auth(s.handleListApps))
	mux.Handle("GET /v1/apps/{app}/status", s.auth(s.handleStatus))
	mux.Handle("GET /v1/apps/{app}/logs", s.auth(s.handleLogs))
	mux.Handle("GET /v1/apps/{app}/env", s.auth(s.handleGetEnv))
	mux.Handle("PUT /v1/apps/{app}/env", s.auth(s.handlePutEnv))
	mux.Handle("POST /v1/apps/{app}/addons", s.auth(s.handleAddAddon))
	mux.Handle("DELETE /v1/apps/{app}/addons/{addon}", s.auth(s.handleRemoveAddon))
	mux.Handle("DELETE /v1/apps/{app}", s.auth(s.handleDestroy))
	mux.Handle("GET /v1/doctor", s.auth(s.handleDoctor))
	mux.Handle("POST /v1/ai/settings", s.auth(s.handleAISettings))
	mux.Handle("POST /v1/ai/build", s.auth(s.handleAIBuild))
	// Everything else is the admin console.
	mux.Handle("/", uiHandler())

	log.Printf("donkey-server listening on %s (domain *.%s)", s.cfg.Listen, s.cfg.BaseDomain)
	return http.ListenAndServe(s.cfg.Listen, mux)
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing API token — run `donkey login`"})
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
