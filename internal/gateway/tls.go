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
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// The gateway is its own certificate authority story: no cert-manager, no
// ingress vendor. Three ways to a certificate —
//
//   - acme:   one certificate per hostname, issued on demand (and pre-issued
//     the moment a route appears). The routing table doubles as the
//     issuance allowlist, so nobody can make the gateway request
//     certificates for names donkey doesn't serve.
//   - ca:     a self-signed CA the gateway mints once and keeps in a Secret,
//     signing per-hostname certificates on the fly — for demo and
//     internal installs ACME can't reach. Trust the one CA certificate
//     and every app is covered.
//   - static: a provided certificate (usually a wildcard) from a Secret.
//
// Issued certificates and the ACME account live in a Secret, so they
// survive pod restarts and are shared by replicas.

// edgeTLS builds the https listener's TLS config and the plain listener's
// handler (the https redirect — wrapped to also answer ACME HTTP-01
// challenges in acme mode).
func (s *Server) edgeTLS(ctx context.Context) (*tls.Config, http.Handler, error) {
	redirect := http.HandlerFunc(redirectHTTPS)
	switch s.cfg.TLSMode {
	case "acme":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Email:      s.cfg.ACMEEmail,
			HostPolicy: s.allowHost,
			Cache:      s.certCache(),
		}
		if s.cfg.ACMEDirectory != "" {
			m.Client = &acme.Client{DirectoryURL: s.cfg.ACMEDirectory}
		}
		// Deploys shouldn't leave the first visitor waiting on a handshake
		// that triggers issuance — new hostnames get their certificate as
		// soon as their route appears. The synthetic hello must advertise
		// ECDSA: autocert keys its cache by certificate type, and a bare
		// hello would pre-issue an RSA certificate no real browser asks
		// for — every visitor would then wait through a second issuance.
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

	case "static":
		cert, err := s.loadStaticCert(ctx)
		if err != nil {
			return nil, nil, err
		}
		return &tls.Config{
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
			Certificates: []tls.Certificate{cert},
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
// actually serves. The routing table is the truth; the base domain and the
// console's host are always entitled (the central login callback lives on
// the base domain even when no app does).
func (s *Server) allowHost(ctx context.Context, host string) error {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return fmt.Errorf("no hostname")
	}
	if host == s.cfg.Domain || host == s.cfg.ServerHost {
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
		if err := s.refreshRoutes(ctx); err == nil {
			s.mu.RLock()
			n = len(s.routes[host])
			s.mu.RUnlock()
		}
	}
	return n > 0
}

// certCache is where issued certificates and the ACME account live: a
// Secret on Kubernetes, a folder of the state directory on Docker.
func (s *Server) certCache() autocert.Cache {
	if s.cfg.Runtime == "docker" {
		return autocert.DirCache(filepath.Join(s.cfg.StateDir, "certs"))
	}
	return &secretCache{
		client:    s.k8s,
		namespace: s.cfg.SystemNamespace,
		name:      manifests.GatewayCertsSecret,
	}
}

// --- Secret-backed autocert cache ----------------------------------------

// secretCache stores autocert's entries (certificates, the account key,
// pending challenges) as the data of one Secret. Replicas share it, and a
// restarted pod finds its certificates where it left them.
type secretCache struct {
	client    *k8s.Client
	namespace string
	name      string
}

// cacheKey maps an autocert key onto a legal Secret data key: autocert uses
// "+" (acme_account+key, <domain>+token), which Secret keys may not contain.
// The replacement token cannot appear in domain names, so keys stay unique.
func cacheKey(key string) string { return strings.ReplaceAll(key, "+", ".plus.") }

func (c *secretCache) Get(ctx context.Context, key string) ([]byte, error) {
	sec, err := c.client.Clientset.CoreV1().Secrets(c.namespace).Get(ctx, c.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, autocert.ErrCacheMiss
	}
	if err != nil {
		return nil, err
	}
	b, ok := sec.Data[cacheKey(key)]
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	return b, nil
}

func (c *secretCache) Put(ctx context.Context, key string, data []byte) error {
	return c.update(ctx, func(m map[string][]byte) {
		m[cacheKey(key)] = data
	})
}

func (c *secretCache) Delete(ctx context.Context, key string) error {
	return c.update(ctx, func(m map[string][]byte) {
		delete(m, cacheKey(key))
	})
}

// update read-modify-writes the Secret, retrying (with a little air between
// attempts) the races two replicas or a burst of first-install issuances run
// into. One Secret caps out around 1 MiB — roughly 250 hostnames — which is
// far beyond this PoC's reach; past it, certificates still serve from
// memory but stop persisting.
func (c *secretCache) update(ctx context.Context, mutate func(map[string][]byte)) error {
	secrets := c.client.Clientset.CoreV1().Secrets(c.namespace)
	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
			}
		}
		sec, err := secrets.Get(ctx, c.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: c.namespace},
				Data:       map[string][]byte{},
			}
			mutate(fresh.Data)
			if _, err := secrets.Create(ctx, fresh, metav1.CreateOptions{}); err == nil {
				return nil
			} else if !apierrors.IsAlreadyExists(err) {
				return err
			}
			lastErr = err
			continue // somebody else created it first — merge into theirs
		}
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		mutate(sec.Data)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// --- Built-in CA ----------------------------------------------------------

// caIssuer signs one certificate per hostname with a self-signed CA — the
// local/internal answer where ACME can't reach. The CA keypair lives in a
// Secret; leaves are minted in memory (a restart simply mints them again).
type caIssuer struct {
	caCert *x509.Certificate
	caDER  []byte
	caKey  *ecdsa.PrivateKey
	allow  func(ctx context.Context, host string) error

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func (s *Server) loadOrCreateCA(ctx context.Context) (*caIssuer, error) {
	certPEM, keyPEM, err := s.readCA(ctx)
	if err != nil {
		return nil, err
	}
	if certPEM == nil {
		certPEM, keyPEM, err = generateCA()
		if err != nil {
			return nil, err
		}
		stored, storedKey, werr := s.writeCA(ctx, certPEM, keyPEM)
		if werr != nil {
			return nil, werr
		}
		// A replica may have won the race — its CA is the CA.
		certPEM, keyPEM = stored, storedKey
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("the stored CA doesn't hold a PEM ca.crt/ca.key pair")
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

// readCA returns the stored CA pair, or nils when none exists yet.
func (s *Server) readCA(ctx context.Context) (certPEM, keyPEM []byte, err error) {
	if s.cfg.Runtime == "docker" {
		cert, cerr := os.ReadFile(filepath.Join(s.cfg.StateDir, "ca", "ca.crt"))
		if errors.Is(cerr, fs.ErrNotExist) {
			return nil, nil, nil
		}
		if cerr != nil {
			return nil, nil, cerr
		}
		key, kerr := os.ReadFile(filepath.Join(s.cfg.StateDir, "ca", "ca.key"))
		if kerr != nil {
			return nil, nil, kerr
		}
		return cert, key, nil
	}
	sec, err := s.k8s.Clientset.CoreV1().Secrets(s.cfg.SystemNamespace).Get(ctx, manifests.GatewayCASecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return sec.Data["ca.crt"], sec.Data["ca.key"], nil
}

// writeCA stores a fresh CA pair and returns what is actually stored — the
// racing replica's, when one got there first.
func (s *Server) writeCA(ctx context.Context, certPEM, keyPEM []byte) ([]byte, []byte, error) {
	if s.cfg.Runtime == "docker" {
		dir := filepath.Join(s.cfg.StateDir, "ca")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), certPEM, 0o644); err != nil {
			return nil, nil, err
		}
		return certPEM, keyPEM, nil
	}
	secrets := s.k8s.Clientset.CoreV1().Secrets(s.cfg.SystemNamespace)
	fresh := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: manifests.GatewayCASecret, Namespace: s.cfg.SystemNamespace},
		Data:       map[string][]byte{"ca.crt": certPEM, "ca.key": keyPEM},
	}
	if _, err := secrets.Create(ctx, fresh, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, nil, err
		}
		sec, gerr := secrets.Get(ctx, manifests.GatewayCASecret, metav1.GetOptions{})
		if gerr != nil {
			return nil, nil, gerr
		}
		return sec.Data["ca.crt"], sec.Data["ca.key"], nil
	}
	return certPEM, keyPEM, nil
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

// --- Static certificate ---------------------------------------------------

// loadStaticCert reads the provided certificate (usually a wildcard): from
// its Secret on Kubernetes, from <state>/tls/tls.crt+tls.key on Docker.
// Rotating it means updating the source and restarting the gateway.
func (s *Server) loadStaticCert(ctx context.Context) (tls.Certificate, error) {
	if s.cfg.Runtime == "docker" {
		dir := filepath.Join(s.cfg.StateDir, "tls")
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("reading the TLS certificate from %s: %w", dir, err)
		}
		return cert, nil
	}
	sec, err := s.k8s.Clientset.CoreV1().Secrets(s.cfg.SystemNamespace).Get(ctx, s.cfg.TLSSecret, metav1.GetOptions{})
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reading the TLS certificate from Secret %s/%s: %w", s.cfg.SystemNamespace, s.cfg.TLSSecret, err)
	}
	cert, err := tls.X509KeyPair(sec.Data["tls.crt"], sec.Data["tls.key"])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("the Secret %s/%s doesn't hold a valid tls.crt/tls.key pair: %w", s.cfg.SystemNamespace, s.cfg.TLSSecret, err)
	}
	return cert, nil
}
