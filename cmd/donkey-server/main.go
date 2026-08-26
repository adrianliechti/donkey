package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/adrianliechti/donkey/internal/aiproxy"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/gateway"
	"github.com/adrianliechti/donkey/internal/server"
	"github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// One binary, three roles: the control plane by default, the login gateway
// with `gateway`, the openai addon's endpoint with `ai`. Same image, three
// Deployments — donkey-ai is separate because it is the one endpoint tenant
// code is meant to reach, and it must not be the process that can write to
// the cluster.
func main() {
	// SIGTERM starts a drain rather than killing in-flight work: a
	// control-plane upgrade must not abandon somebody's half-finished build,
	// and the gateway finishes the responses it is already streaming.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == "gateway" {
		cfg, err := gateway.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "configuration error:", err)
			os.Exit(1)
		}
		gw, err := gateway.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
		if err := gw.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "ai" {
		cfg, err := aiproxy.ConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "configuration error:", err)
			os.Exit(1)
		}
		provider, err := aiRuntime()
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
		if err := aiproxy.Run(ctx, cfg, provider); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, err := server.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	// The control plane runs against one runtime provider — DONKEY_RUNTIME
	// picks which; everything else is identical.
	var rt runtime.Provider
	switch os.Getenv("DONKEY_RUNTIME") {
	case "docker":
		provider, err := docker.New(docker.Options{
			Domain:       cfg.Domain,
			StateDir:     os.Getenv("DONKEY_STATE_DIR"),
			Registry:     cfg.Registry,
			AddonImages:  cfg.AddonImages,
			PullRegistry: os.Getenv("DONKEY_PULL_REGISTRY"),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
		rt = provider
		// Any recreation of donkey's own containers drops the space networks
		// they were on, and Docker's DNS only answers for networks the asking
		// container is attached to — so without this the gateway silently
		// stops resolving every app deployed before the last install.
		go provider.ReconcileOwnNetworks(ctx, time.Minute)
		// The console is one more entry in the gateway's routing table; on
		// Docker the server writes its own at startup (on Kubernetes the
		// installer renders it).
		serverHost := os.Getenv("DONKEY_SERVER_HOST")
		if serverHost == "" {
			serverHost = cfg.Domain
		}
		stateDir := os.Getenv("DONKEY_STATE_DIR")
		if stateDir == "" {
			stateDir = "/var/lib/donkey"
		}
		if err := docker.WriteSystemRoute(stateDir, "console", docker.Route{
			Host: serverHost, Path: "/", Backend: "http://donkey-server:8088",
			Login: true, Access: cfg.Admins, Token: true,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
	default:
		client, err := kubernetes.NewInCluster()
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
		rt = kubernetes.NewRuntime(client, kubernetes.Options{
			Placement: manifests.Placement{
				SharedNamespace: cfg.WorkloadNamespace,
				NamespacePrefix: cfg.NamespacePrefix,
			},
			SystemNamespace:       cfg.SystemNamespace,
			Domain:                cfg.Domain,
			Registry:              cfg.Registry,
			AddonImages:           cfg.AddonImages,
			AddonImagePullSecrets: cfg.AddonImagePullSecrets,
		})
	}
	srv, err := server.New(cfg, rt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error:", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// aiRuntime is the provider donkey-ai reads the per-space tokens through —
// the same runtimes the server supports, with none of its configuration: the
// AI endpoint needs a way to list binding secrets and nothing else.
func aiRuntime() (runtime.Provider, error) {
	switch os.Getenv("DONKEY_RUNTIME") {
	case "docker":
		return docker.New(docker.Options{StateDir: os.Getenv("DONKEY_STATE_DIR")})
	default:
		client, err := kubernetes.NewInCluster()
		if err != nil {
			return nil, err
		}
		// Only the two namespaces matter here: they are what the binding
		// sweep reads. donkey-ai never maps a space onto a namespace, so the
		// prefix is none of its business.
		return kubernetes.NewRuntime(client, kubernetes.Options{
			Placement:       manifests.Placement{SharedNamespace: os.Getenv("DONKEY_WORKLOAD_NAMESPACE")},
			SystemNamespace: envOr("DONKEY_SYSTEM_NAMESPACE", config.DefaultSystemNamespace),
		}), nil
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
