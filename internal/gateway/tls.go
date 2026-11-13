package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// The gateway has three ways to a certificate —
//
//   - cert-manager: exact-host certificates arrive as labelled TLS Secrets.
//   - acme:   one per hostname, issued on demand and pre-issued when a route
//     appears. The routing table doubles as the issuance allowlist.
//   - ca:     a persisted self-signed CA, signing per-hostname
//     certificates on the fly — for installs ACME cannot reach.
//
// Runtime adapters persist ACME/account state and the built-in CA. The
// gateway handles TLS without knowing the storage or runtime object format.

// edgeTLS builds the https listener's TLS config and the plain listener's
// handler (the https redirect — wrapped to also answer ACME HTTP-01
// challenges in acme mode).
func (s *Server) edgeTLS(ctx context.Context) (*tls.Config, http.Handler, error) {
	redirect := http.HandlerFunc(redirectHTTPS)
	if err := s.tlsStore.PrepareGateway(ctx); err != nil {
		return nil, nil, fmt.Errorf("preparing gateway TLS: %w", err)
	}
	switch s.cfg.TLSMode {
	case "cert-manager":
		return s.managedCertificateTLS(ctx)
	case "acme":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Email:      s.cfg.ACMEEmail,
			HostPolicy: s.allowHost,
			Cache:      s.tlsStore.GatewayCertificateCache(),
		}
		if s.cfg.ACMEDirectory != "" {
			m.Client = &acme.Client{DirectoryURL: s.cfg.ACMEDirectory}
		}
		// New hostnames get their certificate when the route appears, not on
		// the first visitor's handshake. The synthetic hello must advertise
		// ECDSA: autocert keys its cache by certificate type, so a bare hello
		// pre-issues an RSA certificate no browser asks for, and every visitor
		// waits through a second issuance anyway.
		s.warm = func(host string) {
			hello := &tls.ClientHelloInfo{
				ServerName:       host,
				CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
				SupportedCurves:  []tls.CurveID{tls.CurveP256},
				SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			}
			if _, err := m.GetCertificate(hello); err != nil {
				log.Printf("tls: pre-issuing %s: %v", host, err)
			}
		}
		return m.TLSConfig(), m.HTTPHandler(redirect), nil

	case "ca":
		issuer, err := s.loadOrCreateCA(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("setting up the built-in CA: %w", err)
		}
		return &tls.Config{
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"h2", "http/1.1"},
			GetCertificate: issuer.getCertificate,
		}, redirect, nil
	}
	return nil, nil, fmt.Errorf("unknown TLS mode %q", s.cfg.TLSMode)
}

// redirectHTTPS sends every plain-http visitor to https. ACME challenges
// never get here — in acme mode autocert's handler answers them first.
func redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+requestHost(r)+r.URL.RequestURI(), http.StatusMovedPermanently)
}

// allowHost is the issuance gate: certificates only for names donkey
// actually serves. The routing table is the truth; the base domain, the
// console's host and the machines' address are always entitled (the central
// login callback lives on the base domain even when no app does, and the
// machines' address tells a visitor whose machine is stopped that it is).
func (s *Server) allowHost(ctx context.Context, host string) error {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return fmt.Errorf("no hostname")
	}
	if host == s.cfg.Domain || host == s.cfg.ServerHost || host == s.cfg.machineHost() {
		return nil
	}
	if s.hostServed(ctx, host) {
		return nil
	}
	return fmt.Errorf("no app serves %s", host)
}

// hostServed says whether any route serves the hostname, refreshing once
// (rate-limited) on a miss — a hostname deployed seconds ago must already
// be issuable.
func (s *Server) hostServed(ctx context.Context, host string) bool {
	s.mu.RLock()
	n := len(s.routes[host])
	stale := time.Since(s.lastAttempt) > 2*time.Second
	s.mu.RUnlock()
	if n > 0 {
		return true
	}
	if stale {
		if err := s.refreshRoutesAfter(ctx, 2*time.Second); err == nil {
			s.mu.RLock()
			n = len(s.routes[host])
			s.mu.RUnlock()
		}
	}
	return n > 0
}

// --- Built-in CA ----------------------------------------------------------

// caIssuer signs one certificate per hostname with a self-signed CA — the
// local/internal answer where ACME can't reach. The runtime adapter persists
// the CA keypair; leaves are minted in memory on demand after each restart.
type caIssuer struct {
	caCert *x509.Certificate
	caDER  []byte
	caKey  *ecdsa.PrivateKey
	allow  func(ctx context.Context, host string) error

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func (s *Server) loadOrCreateCA(ctx context.Context) (*caIssuer, error) {
	certPEM, keyPEM, err := s.tlsStore.GatewayCA(ctx, generateCA)
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("the stored CA doesn't hold a PEM certificate and private key")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return &caIssuer{
		caCert: caCert,
		caDER:  certBlock.Bytes,
		caKey:  caKey,
		allow:  s.allowHost,
		leaves: map[string]*tls.Certificate{},
	}, nil
}

func generateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "donkey CA", Organization: []string{"donkey"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func (i *caIssuer) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if host == "" {
		return nil, fmt.Errorf("the client sent no server name")
	}
	if err := i.allow(hello.Context(), host); err != nil {
		return nil, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	// A cached leaf serves until close to expiry, then gets minted anew.
	if leaf, ok := i.leaves[host]; ok && time.Until(leaf.Leaf.NotAfter) > 24*time.Hour {
		return leaf, nil
	}
	leaf, err := i.mint(host)
	if err != nil {
		return nil, err
	}
	i.leaves[host] = leaf
	return leaf, nil
}

func (i *caIssuer) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		// Under 398 days, so even browsers that enforce the public-CA limit
		// on private roots have nothing to complain about.
		NotAfter:    time.Now().AddDate(0, 0, 397),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, i.caCert, &key.PublicKey, i.caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, i.caDER},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}
