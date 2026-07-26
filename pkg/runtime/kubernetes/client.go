// Package k8s wraps all Kubernetes access. Everything goes through the user's
// kubeconfig and client-go — donkey never shells out to kubectl.
package kubernetes

import (
	"context"
	"fmt"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// restConfig builds a client config for the named kubeconfig context.
func restConfig(kubeContext string) (*rest.Config, error) {
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig context %q: %w", kubeContext, err)
	}
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg, nil
}

// Ping contacts the cluster and returns its version string.
func Ping(ctx context.Context, kubeContext string) (string, error) {
	cfg, err := restConfig(kubeContext)
	if err != nil {
		return "", err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return "", err
	}
	v, err := disco.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// Client bundles the two client flavors donkey uses: dynamic for server-side
// apply of anything, typed for reads (pods, logs, rollout status).
type Client struct {
	Dynamic   dynamic.Interface
	Clientset k8sclient.Interface
	Config    *rest.Config
}

func New(kubeContext string) (*Client, error) {
	cfg, err := restConfig(kubeContext)
	if err != nil {
		return nil, err
	}
	return fromRest(cfg)
}

// NewInCluster is used by donkey-server, which runs inside the cluster it
// manages.
func NewInCluster() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster credentials: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100
	return fromRest(cfg)
}

func fromRest(cfg *rest.Config) (*Client, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{Dynamic: dyn, Clientset: cs, Config: cfg}, nil
}
