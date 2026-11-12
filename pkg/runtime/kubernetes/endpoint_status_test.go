package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

type fixedDNSResolver struct {
	ips   []net.IP
	cname string
	err   error
}

func (r fixedDNSResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return r.ips, r.err
}

func (r fixedDNSResolver) LookupCNAME(context.Context, string) (string, error) {
	return r.cname, r.err
}

func edgeObject(apiVersion, kind, name, namespace, installation, space, app string) *unstructured.Unstructured {
	labels := manifests.Labels(space, app, manifests.RoleEdge)
	labels[manifests.LabelInstallation] = installation
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":       name,
			"namespace":  namespace,
			"generation": int64(1),
			"labels":     stringMapToAny(labels),
		},
	}}
}

func stringMapToAny(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func TestEndpointStatusReadsCertificateAndDNSReality(t *testing.T) {
	const (
		installation = "bank-donkey-system"
		space        = "acme"
		app          = "shop"
		host         = "shop.acme.az.example.com"
	)
	expires := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	renews := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)

	certificate := edgeObject("cert-manager.io/v1", "Certificate",
		manifests.AppCertificateName(space, app), installation, installation, space, app)
	certificate.Object["status"] = map[string]any{
		"notAfter":    expires.Format(time.RFC3339),
		"renewalTime": renews.Format(time.RFC3339),
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "Ready",
			"message":            "Certificate is up to date and has not expired",
			"observedGeneration": int64(1),
		}},
	}
	dns := edgeObject("externaldns.k8s.io/v1alpha1", "DNSEndpoint",
		manifests.AppDNSName(space, app), installation, installation, space, app)
	dns.Object["spec"] = map[string]any{"endpoints": []any{map[string]any{
		"dnsName": host, "recordType": "A", "targets": []any{"192.0.2.10"},
	}}}
	dns.Object["status"] = map[string]any{"observedGeneration": int64(1)}

	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), certificate, dns)
	r := NewRuntime(&Client{Clientset: fake.NewSimpleClientset(), Dynamic: dynamic}, Options{
		SystemNamespace: installation,
		TLS: config.InstallTLS{
			Mode: "cert-manager", IssuerName: "platform-ca",
		},
		ExternalDNS: config.InstallExternalDNS{Targets: []string{"192.0.2.10"}},
	})
	r.resolver = fixedDNSResolver{ips: []net.IP{net.ParseIP("192.0.2.10")}}

	status := r.EndpointStatus(context.Background(), space, app, host)
	if status.Certificate.State != rt.EndpointReady || status.Certificate.Mode != "cert-manager" {
		t.Fatalf("certificate status = %+v, want ready cert-manager", status.Certificate)
	}
	if !status.Certificate.ExpiresAt.Equal(expires) || !status.Certificate.RenewalTime.Equal(renews) {
		t.Errorf("certificate dates = %v / %v, want %v / %v",
			status.Certificate.ExpiresAt, status.Certificate.RenewalTime, expires, renews)
	}
	if status.DNS.State != rt.EndpointReady || !status.DNS.Observed {
		t.Fatalf("DNS status = %+v, want observed and ready", status.DNS)
	}
	if !slices.Equal(status.DNS.Expected, []string{"192.0.2.10"}) {
		t.Errorf("DNS expected = %v", status.DNS.Expected)
	}
}

func TestEndpointStatusSurfacesIssuanceFailureAndDNSPropagation(t *testing.T) {
	const (
		installation = "donkey-system"
		space        = "team"
		app          = "api"
		host         = "api.team.example.com"
	)
	certificate := edgeObject("cert-manager.io/v1", "Certificate",
		manifests.AppCertificateName(space, app), installation, installation, space, app)
	certificate.Object["status"] = map[string]any{
		"failedIssuanceAttempts": int64(2),
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "Failed",
			"message": "The issuer refused the request", "observedGeneration": int64(1),
		}},
	}
	dns := edgeObject("externaldns.k8s.io/v1alpha1", "DNSEndpoint",
		manifests.AppDNSName(space, app), installation, installation, space, app)
	dns.Object["spec"] = map[string]any{"endpoints": []any{map[string]any{
		"dnsName": host, "recordType": "A", "targets": []any{"10.0.0.5"},
	}}}
	dns.Object["status"] = map[string]any{"observedGeneration": int64(1)}

	r := NewRuntime(&Client{
		Clientset: fake.NewSimpleClientset(),
		Dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), certificate, dns),
	}, Options{
		SystemNamespace: installation,
		TLS:             config.InstallTLS{Mode: "cert-manager", IssuerName: "broken"},
		ExternalDNS:     config.InstallExternalDNS{Targets: []string{"10.0.0.5"}},
	})
	r.resolver = fixedDNSResolver{ips: []net.IP{net.ParseIP("10.0.0.4")}}

	status := r.EndpointStatus(context.Background(), space, app, host)
	if status.Certificate.State != rt.EndpointError || status.Certificate.Reason != "Failed" ||
		!strings.Contains(status.Certificate.Message, "refused") {
		t.Errorf("certificate status did not preserve the failure: %+v", status.Certificate)
	}
	if status.DNS.State != rt.EndpointPropagating || !strings.Contains(status.DNS.Message, "10.0.0.4") {
		t.Errorf("DNS status did not show the stale answer: %+v", status.DNS)
	}
}

func TestGatewayIssuedCertificateModesReportTheirStoredReadiness(t *testing.T) {
	const (
		installation = "donkey-system"
		host         = "shop.acme.example.com"
	)
	labels := manifests.SystemLabels(manifests.GatewayAppName)
	labels[manifests.LabelInstallation] = installation
	labels[manifests.LabelInstance] = installation

	acmeCert, acmeKey := testKeypair(t, host, false)
	caCert, caKey := testKeypair(t, "donkey CA", true)
	// autocert stores the private key before its certificate chain.
	acmeBundle := append(acmeKey, acmeCert...)

	objects := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: manifests.GatewayCertsSecret, Namespace: installation, Labels: labels,
		}, Data: map[string][]byte{host: acmeBundle}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: manifests.GatewayCASecret, Namespace: installation, Labels: labels,
		}, Data: map[string][]byte{"ca.crt": caCert, "ca.key": caKey}},
	}
	client := &Client{Clientset: fake.NewSimpleClientset(objects...)}

	acme := NewRuntime(client, Options{SystemNamespace: installation, TLS: config.InstallTLS{Mode: "acme"}})
	acmeStatus := acme.certificateStatus(context.Background(), "acme", "shop", host)
	if acmeStatus.State != rt.EndpointReady || acmeStatus.ExpiresAt.IsZero() {
		t.Fatalf("ACME status = %+v, want cached and ready", acmeStatus)
	}

	ca := NewRuntime(client, Options{SystemNamespace: installation, TLS: config.InstallTLS{Mode: "ca"}})
	caStatus := ca.certificateStatus(context.Background(), "acme", "shop", host)
	if caStatus.State != rt.EndpointReady || !caStatus.ExpiresAt.IsZero() {
		// The CA lifetime is validated but intentionally not presented as the
		// per-host leaf's expiry date.
		t.Fatalf("CA status = %+v, want ready without the CA's expiry as leaf metadata", caStatus)
	}
}

func testKeypair(t *testing.T, name string, isCA bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         isCA,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
		template.BasicConstraintsValid = true
	} else {
		template.DNSNames = []string{name}
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestDNSStatusDistinguishesMissingAnswerFromLookupFailure(t *testing.T) {
	const (
		installation = "donkey-system"
		space        = "team"
		app          = "api"
		host         = "api.team.example.com"
	)
	dns := edgeObject("externaldns.k8s.io/v1alpha1", "DNSEndpoint",
		manifests.AppDNSName(space, app), installation, installation, space, app)
	dns.Object["spec"] = map[string]any{"endpoints": []any{map[string]any{
		"dnsName": host, "recordType": "A", "targets": []any{"10.0.0.5"},
	}}}
	r := NewRuntime(&Client{
		Clientset: fake.NewSimpleClientset(),
		Dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), dns),
	}, Options{
		SystemNamespace: installation,
		ExternalDNS:     config.InstallExternalDNS{Targets: []string{"10.0.0.5"}},
	})

	r.resolver = fixedDNSResolver{err: &net.DNSError{Err: "no such host", IsNotFound: true}}
	if got := r.dnsStatus(context.Background(), space, app, host); got.State != rt.EndpointPropagating {
		t.Fatalf("NXDOMAIN status = %+v, want propagating", got)
	}
	r.resolver = fixedDNSResolver{err: context.DeadlineExceeded}
	if got := r.dnsStatus(context.Background(), space, app, host); got.State != rt.EndpointUnknown ||
		!strings.Contains(got.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("resolver failure status = %+v, want unknown with the error", got)
	}
}

func TestResolveDNSHandlesEveryConfiguredRecordType(t *testing.T) {
	tests := []struct {
		name       string
		recordType string
		resolver   fixedDNSResolver
		want       []string
	}{
		{name: "A", recordType: "A", resolver: fixedDNSResolver{
			ips: []net.IP{net.ParseIP("192.0.2.10")},
		}, want: []string{"192.0.2.10"}},
		{name: "AAAA", recordType: "AAAA", resolver: fixedDNSResolver{
			ips: []net.IP{net.ParseIP("2001:0db8::5")},
		}, want: []string{"2001:db8::5"}},
		{name: "CNAME", recordType: "CNAME", resolver: fixedDNSResolver{
			cname: "Gateway.Example.COM.",
		}, want: []string{"gateway.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDNS(context.Background(), tt.resolver, "app.example.com", tt.recordType)
			if err != nil {
				t.Fatal(err)
			}
			if got = normalizedValues(got); !slices.Equal(got, tt.want) {
				t.Fatalf("resolved %s = %v, want %v", tt.recordType, got, tt.want)
			}
		})
	}
}
