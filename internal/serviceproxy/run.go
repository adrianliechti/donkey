package serviceproxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
)

type Config struct {
	Listen   string
	Services config.ProxyServices
	File     string
	TestKey  []byte
}

func FromEnv() (*Config, error) {
	cfg := &Config{Listen: os.Getenv("DONKEY_LISTEN"), File: os.Getenv("DONKEY_PROXY_SERVICES_FILE")}
	if cfg.Listen == "" {
		cfg.Listen = ":8091"
	}
	if raw := os.Getenv("DONKEY_PROXY_SERVICES"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Services); err != nil {
			return nil, fmt.Errorf("DONKEY_PROXY_SERVICES must be a JSON array of proxy service configurations")
		}
	}
	if raw := os.Getenv("DONKEY_TELEMETRY_KEY"); raw != "" {
		var err error
		if cfg.TestKey, err = hex.DecodeString(raw); err != nil || len(cfg.TestKey) != 32 {
			return nil, fmt.Errorf("DONKEY_TELEMETRY_KEY must be 32 bytes of hex")
		}
	}
	return cfg, cfg.Services.Validate()
}

func Run(ctx context.Context, cfg *Config, provider runtime.Provider) error {
	handler, err := handlerWithTests(cfg.Services, provider.ServiceProxyAccess, provider.ServiceProxySpaceAccess, cfg.TestKey)
	if err != nil {
		return err
	}
	var dynamic *reloadHandler
	if cfg.File != "" {
		// On Docker the file is a state record the server rewrites: read as
		// services.yaml first, then a services.json left by older versions —
		// the path installations configured.
		dynamic = &reloadHandler{read: func() ([]byte, error) { return docker.ReadRecordFile(cfg.File) }, access: provider.ServiceProxyAccess, spaceAccess: provider.ServiceProxySpaceAccess, testKey: cfg.TestKey, interval: 30 * time.Second}
		handler = dynamic
	}
	srv := &http.Server{Addr: cfg.Listen, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
			if r.URL.Path == "/readyz" && dynamic != nil {
				if _, err := dynamic.load(); err != nil {
					http.Error(w, "proxy configuration unavailable", http.StatusServiceUnavailable)
					return
				}
			}
			w.Write([]byte("ok"))
			return
		}
		handler.ServeHTTP(w, r)
	})}
	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-proxy listening on %s", cfg.Listen)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return srv.Shutdown(drain)
}

// reloadHandler swaps complete, validated configurations. A read or validation
// failure refuses requests instead of serving a target that may have been revoked.
type reloadHandler struct {
	mu          sync.Mutex
	read        func() ([]byte, error)
	access      Access
	spaceAccess Access
	testKey     []byte
	interval    time.Duration
	until       time.Time
	raw         string
	current     http.Handler
	err         error
}

func (h *reloadHandler) load() (http.Handler, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil || time.Now().After(h.until) {
		h.until = time.Now().Add(h.interval)
		data, err := h.read()
		if err == nil && (h.current == nil || string(data) != h.raw) {
			// YAML, or JSON as Kubernetes mounts it — YAML reads both.
			var services config.ProxyServices
			err = yaml.Unmarshal(data, &services)
			if err == nil {
				var fresh http.Handler
				fresh, err = handlerWithTests(services, h.access, h.spaceAccess, h.testKey)
				if err == nil {
					h.current, h.raw = fresh, string(data)
				}
			}
		}
		h.err = err
	}
	return h.current, h.err
}

func (h *reloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	current, err := h.load()
	if err != nil || current == nil {
		http.Error(w, "proxy configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	current.ServeHTTP(w, r)
}
