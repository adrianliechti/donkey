package main

import (
	"fmt"

	"github.com/adrianliechti/donkey/internal/gateway"
	"github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
)

// Runtime selection belongs in the composition root. The gateway consumes
// interfaces and cannot construct Kubernetes clients or Docker adapters.
func gatewayBackend(cfg *gateway.Config) (runtime.GatewayBackend, error) {
	switch cfg.Runtime {
	case "docker":
		return &docker.GatewayAdapter{StateDir: cfg.StateDir}, nil
	case "kubernetes":
		client, err := kubernetes.NewInCluster()
		if err != nil {
			return nil, err
		}
		return &kubernetes.GatewayAdapter{Client: client.ForInstallation(cfg.SystemNamespace, cfg.WorkloadNamespace), Namespace: cfg.SystemNamespace}, nil
	default:
		return nil, fmt.Errorf("unknown gateway runtime %q", cfg.Runtime)
	}
}
