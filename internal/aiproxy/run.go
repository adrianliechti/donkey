package aiproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// Config is what donkey-ai reads from its environment:
// the endpoints it fronts, the credentials it swaps in, and where to look for
// the per-space tokens. No session key, no registry, no builder — this
// process holds nothing the admin API holds.
type Config struct {
	Listen string
	// UpstreamURL/UpstreamKey are the platform's own AI endpoint and
	// credential. The credential never travels past this process.
	UpstreamURL string
	UpstreamKey string
	ExaURL      string
	ExaKey      string
}

func ConfigFromEnv() (*Config, error) {
	c := &Config{
		Listen:      envOr("DONKEY_LISTEN", ":8089"),
		UpstreamURL: os.Getenv("OPENAI_BASE_URL"),
		UpstreamKey: os.Getenv("OPENAI_API_KEY"),
		ExaURL:      os.Getenv("EXA_BASE_URL"),
		ExaKey:      os.Getenv("EXA_API_KEY"),
	}
	if c.UpstreamKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required — it is the credential donkey-ai swaps each space's token for")
	}
	if err := (config.InstallExa{BaseURL: c.ExaURL}).Validate(); err != nil {
		return nil, err
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
// the valid per-space tokens — the only thing this process asks the cluster
// for, and read-only.
func Run(ctx context.Context, cfg *Config, provider rt.Provider) error {
	handler, err := Handler(cfg.UpstreamURL, cfg.UpstreamKey, provider.AIProxyTokens)
	if err != nil {
		return err
	}
	exa, err := ExaHandler(cfg.ExaURL, cfg.ExaKey, provider.ExaProxyTokens)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(ExaPrefix, exa)
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
