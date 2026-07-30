// Package k8s wraps all Kubernetes access. Everything goes through the user's
// kubeconfig and client-go — donkey never shells out to kubectl.
package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
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

	// Instance is the installation this client speaks for — the control
	// plane's namespace, which is unique per installation by construction.
	// Everything applied is stamped with it and every cluster-wide sweep
	// requires it, so two donkeys on one cluster stay out of each other's
	// objects. Empty means the default installation.
	Instance string

	// Namespaces narrows every sweep that would otherwise cross the whole
	// cluster. Empty means the cluster, which is what an installation with a
	// namespace per space needs — and has the rights for. An installation
	// confined to namespaces it was given lists only those: it holds no
	// cluster-scoped permission, so a cluster-wide List would not merely
	// return too much, it would be refused.
	Namespaces []string
}

// ForInstance returns the client bound to one installation, sweeping only the
// given namespaces (none means the whole cluster). The installer and the
// server both call it once they know where the control plane lives.
func (c *Client) ForInstance(instance string, namespaces ...string) *Client {
	out := *c
	out.Instance = instance
	out.Namespaces = nil
	for _, ns := range namespaces {
		if ns != "" {
			out.Namespaces = append(out.Namespaces, ns)
		}
	}
	return &out
}

// ForInstallation is ForInstance spelled from an installation's settings:
// confined to the two namespaces it was given when a workload namespace is
// named, cluster-wide otherwise. The one rule the server and the gateway
// must agree on, so it is written once.
func (c *Client) ForInstallation(system, workload string) *Client {
	if workload != "" {
		return c.ForInstance(system, system, workload)
	}
	return c.ForInstance(system)
}

// sweep is the namespaces a cluster-wide read must visit.
func (c *Client) sweep() []string {
	if len(c.Namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return c.Namespaces
}

// Confined reports whether this installation lives in namespaces it was
// given rather than ones it made. A confined donkey must not create or delete
// a namespace — and must not read one either, since its rights stop at the
// namespace boundary and the attempt would simply be refused.
func (c *Client) Confined() bool { return len(c.Namespaces) > 0 }

// instance is this client's installation, with the default filled in.
func (c *Client) instance() string {
	if c.Instance == "" {
		return manifests.DefaultSystemNamespace
	}
	return c.Instance
}

// ours reports whether an object belongs to this installation. Everything
// donkey applies is stamped on its way out (see stampInstance), so a missing
// label means another writer — never this installation.
func (c *Client) ours(labels map[string]string) bool {
	return labels[manifests.LabelInstance] == c.instance()
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
	// The API server is never reached through a forward proxy. client-go
	// otherwise falls back to http.ProxyFromEnvironment, so an installation
	// that sets build.proxy (for the build engine, and for donkey-server's
	// own reach to a registry on the internet) would send every API call to a
	// proxy that cannot route a cluster service address — which takes the
	// whole control plane down. Saying so here beats trying to spell the
	// service CIDR into NO_PROXY, which differs per cluster.
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
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
