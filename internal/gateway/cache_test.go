package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

type countedRoutes struct {
	calls atomic.Int64
	fail  bool
}

func (c *countedRoutes) list(context.Context) ([]route, error) {
	c.calls.Add(1)
	if c.fail {
		return nil, errors.New("route backend unavailable")
	}
	return []route{{Host: "known.example", Path: "/"}}, nil
}

func concurrentRequests(fn func(int)) {
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); fn(i) }()
	}
	wg.Wait()
}

func TestRouteRefreshDoesNotFollowRequestVolume(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint("failed=", failed), func(t *testing.T) {
			source := &countedRoutes{fail: failed}
			s := testServer()
			s.source = source
			s.routes = map[string][]route{"known.example": {{Host: "known.example", Path: "/"}}}
			ctx := context.Background()
			concurrentRequests(func(int) { s.lookup(ctx, "known.example", "/") })
			if source.calls.Load() != 0 {
				t.Fatal("normal route requests accessed the backend")
			}
			concurrentRequests(func(i int) { s.lookup(ctx, fmt.Sprintf("unknown-%d.example", i), "/") })
			if source.calls.Load() != 1 {
				t.Fatalf("unknown host burst triggered %d reads", source.calls.Load())
			}
			concurrentRequests(func(i int) { s.hostServed(ctx, fmt.Sprintf("sni-%d.example", i)) })
			if source.calls.Load() != 1 {
				t.Fatal("SNI misses bypassed the route refresh throttle")
			}
			s.mu.Lock()
			s.lastAttempt = time.Now().Add(-3 * time.Second)
			s.mu.Unlock()
			concurrentRequests(func(i int) { s.lookup(ctx, fmt.Sprintf("retry-%d.example", i), "/") })
			if source.calls.Load() != 2 {
				t.Fatalf("retry burst triggered %d total reads", source.calls.Load())
			}
		})
	}
}

type countedCertificates struct {
	calls   atomic.Int64
	records []runtime.GatewayCertificate
	fail    bool
}

func (c *countedCertificates) GatewayCertificates(context.Context) ([]runtime.GatewayCertificate, error) {
	c.calls.Add(1)
	if c.fail {
		return nil, errors.New("certificate backend unavailable")
	}
	return c.records, nil
}

func testGatewayCertificate(t *testing.T, host string) runtime.GatewayCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.GatewayCertificate{Name: host, Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})}
}

func TestManagedCertificatesServeFromMemoryAndThrottleMisses(t *testing.T) {
	source := &countedCertificates{records: []runtime.GatewayCertificate{testGatewayCertificate(t, "known.example")}}
	certs := &managedCertificates{source: source, byHost: map[string]*tls.Certificate{}}
	if err := certs.reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	concurrentRequests(func(int) {
		if cert, err := certs.getCertificate(&tls.ClientHelloInfo{ServerName: "KNOWN.EXAMPLE"}); err != nil || cert == nil {
			t.Error("cached certificate unavailable")
		}
	})
	concurrentRequests(func(i int) {
		_, _ = certs.getCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("unknown-%d.example", i)})
	})
	if source.calls.Load() != 1 {
		t.Fatal("cached certificates or misses caused a backend read")
	}
	for _, failed := range []bool{false, true} {
		source.fail = failed
		certs.mu.Lock()
		certs.lastAttempt = time.Now().Add(-3 * time.Second)
		certs.mu.Unlock()
		before := source.calls.Load()
		concurrentRequests(func(i int) {
			_, _ = certs.getCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("unknown-%d.example", i)})
		})
		if source.calls.Load() != before+1 {
			t.Fatal("concurrent TLS misses bypassed throttle")
		}
	}
}
