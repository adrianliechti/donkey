package aiproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// Config holds the machine proxy's listener and its single upstream.
type Config struct {
	Listen string
	// UpstreamURL/UpstreamKey are the platform's own AI endpoint and
	// credential. The credential never travels past this process.
	UpstreamURL string
	UpstreamKey string
}

func ConfigFromEnv() (*Config, error) {
	c := &Config{
		Listen:      envOr("DONKEY_LISTEN", ":8089"),
		UpstreamURL: os.Getenv("OPENAI_BASE_URL"),
		UpstreamKey: os.Getenv("OPENAI_API_KEY"),
	}
	if c.UpstreamKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required — donkey-ai uses it for development machine model requests")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Run serves the proxy until the context is cancelled. The provider supplies
// the valid machine tokens — the only thing this process asks the cluster
// for, and read-only.
func Run(ctx context.Context, cfg *Config, provider rt.Provider) error {
	handler, err := Handler(cfg.UpstreamURL, cfg.UpstreamKey, provider.AIProxyTokens)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(Prefix, handler)
	// Probes answer on the same listener: nothing here is privileged, so
	// there is no admin surface to keep them away from.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		// Model responses stream (SSE) — no write timeout. Headers must
		// still arrive promptly.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-ai listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
