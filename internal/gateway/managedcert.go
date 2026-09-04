package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// managedCertificates is the live view of cert-manager's generated Secrets.
// Certificates and routes change at human speed, so a short poll plus an
// immediate refresh on an unknown SNI keeps the implementation small while
// still making a newly issued app certificate usable on its first request.
type managedCertificates struct {
	server *Server

	mu          sync.RWMutex
	byHost      map[string]*tls.Certificate
	all         []*tls.Certificate
	lastAttempt time.Time
	refresh     singleflight.Group
}

func (s *Server) managedCertificateTLS(ctx context.Context) (*tls.Config, http.Handler, error) {
	if s.k8s == nil {
		return nil, nil, fmt.Errorf("cert-manager TLS needs the Kubernetes runtime")
	}
	certificates := &managedCertificates{server: s, byHost: map[string]*tls.Certificate{}}
	if err := certificates.reload(ctx); err != nil {
		// cert-manager may not have produced the first Secret yet, and a brief
		// API outage must not crash the gateway. The poller and an SNI miss retry.
		log.Printf("initial certificate sync failed (will retry): %v", err)
	}
	go certificates.poll(ctx)
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: certificates.getCertificate,
	}, http.HandlerFunc(redirectHTTPS), nil
}

const certificatePollInterval = 10 * time.Second

func (m *managedCertificates) poll(ctx context.Context) {
	ticker := time.NewTicker(certificatePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.reload(ctx); err != nil {
				log.Printf("certificate sync failed: %v", err)
			}
		}
	}
}

func (m *managedCertificates) reload(ctx context.Context) error {
	_, err, _ := m.refresh.Do("certificates", func() (any, error) {
		m.mu.Lock()
		m.lastAttempt = time.Now()
		m.mu.Unlock()
		selector := fmt.Sprintf("%s=%s", manifests.LabelInstallation, m.server.cfg.SystemNamespace)
		list, err := m.server.k8s.Clientset.CoreV1().Secrets(m.server.cfg.SystemNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return nil, err
		}
		byHost := map[string]*tls.Certificate{}
		var all []*tls.Certificate
		for i := range list.Items {
			secret := &list.Items[i]
			if secret.Labels[manifests.LabelInstallation] != m.server.cfg.SystemNamespace {
				continue
			}
			role := secret.Labels[manifests.LabelRole]
			if role != manifests.RoleEdge && role != manifests.SystemRole(manifests.GatewayAppName) {
				continue
			}
			certPEM, keyPEM := secret.Data["tls.crt"], secret.Data["tls.key"]
			if len(certPEM) == 0 || len(keyPEM) == 0 {
				continue
			}
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				log.Printf("ignoring invalid TLS Secret %s/%s: %v", secret.Namespace, secret.Name, err)
				continue
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				log.Printf("ignoring invalid certificate in Secret %s/%s: %v", secret.Namespace, secret.Name, err)
				continue
			}
			pair.Leaf = leaf
			all = append(all, &pair)
			for _, host := range leaf.DNSNames {
				byHost[normalizeTLSHost(host)] = &pair
			}
		}
		m.mu.Lock()
		m.byHost, m.all = byHost, all
		m.mu.Unlock()
		return nil, nil
	})
	return err
}

func (m *managedCertificates) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := normalizeTLSHost(hello.ServerName)
	if host == "" {
		return nil, fmt.Errorf("the client sent no server name")
	}
	if cert := m.find(host); cert != nil {
		return cert, nil
	}
	m.mu.RLock()
	stale := time.Since(m.lastAttempt) > 2*time.Second
	m.mu.RUnlock()
	if stale {
		if err := m.reload(hello.Context()); err != nil {
			return nil, fmt.Errorf("refreshing cert-manager certificates: %w", err)
		}
		if cert := m.find(host); cert != nil {
			return cert, nil
		}
	}
	return nil, fmt.Errorf("no cert-manager certificate is ready for %s", host)
}

func (m *managedCertificates) find(host string) *tls.Certificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cert := m.byHost[host]; cert != nil {
		return cert
	}
	// Exact SANs are the normal path. This fallback keeps externally managed
	// wildcard Secrets useful without implementing X.509 wildcard rules twice.
	for _, cert := range m.all {
		if cert.Leaf != nil && cert.Leaf.VerifyHostname(host) == nil {
			return cert
		}
	}
	return nil
}

func normalizeTLSHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
