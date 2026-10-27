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

	"github.com/adrianliechti/donkey/pkg/runtime"
	"golang.org/x/sync/singleflight"
)

// managedCertificates is the live view of cert-manager's generated Secrets.
// Certificates and routes change at human speed, so a short poll plus an
// immediate refresh on an unknown SNI keeps the implementation small while
// still making a newly issued app certificate usable on its first request.
type managedCertificates struct {
	source runtime.GatewayCertificateSource

	mu          sync.RWMutex
	byHost      map[string]*tls.Certificate
	all         []*tls.Certificate
	lastAttempt time.Time
	refresh     singleflight.Group
}

func (s *Server) managedCertificateTLS(ctx context.Context) (*tls.Config, http.Handler, error) {
	source, ok := s.tlsStore.(runtime.GatewayCertificateSource)
	if !ok {
		return nil, nil, fmt.Errorf("the configured runtime does not supply managed TLS certificates")
	}
	certificates := &managedCertificates{source: source, byHost: map[string]*tls.Certificate{}}
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
	return m.reloadAfter(ctx, 0)
}

func (m *managedCertificates) reloadAfter(ctx context.Context, minAge time.Duration) error {
	_, err, _ := m.refresh.Do("certificates", func() (any, error) {
		m.mu.Lock()
		if time.Since(m.lastAttempt) < minAge {
			m.mu.Unlock()
			return nil, nil
		}
		m.lastAttempt = time.Now()
		m.mu.Unlock()
		records, err := m.source.GatewayCertificates(ctx)
		if err != nil {
			return nil, err
		}
		byHost := map[string]*tls.Certificate{}
		var all []*tls.Certificate
		for _, record := range records {
			certPEM, keyPEM := record.Certificate, record.PrivateKey
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				log.Printf("ignoring invalid TLS certificate %s: %v", record.Name, err)
				continue
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				log.Printf("ignoring invalid certificate %s: %v", record.Name, err)
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
		if err := m.reloadAfter(hello.Context(), 2*time.Second); err != nil {
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
