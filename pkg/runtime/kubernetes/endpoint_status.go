package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// dnsResolver is the part of net.Resolver endpoint checks use. A narrow
// interface makes the comparison deterministic in tests and keeps DNS an
// observation, not another provider dependency.
type dnsResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// EndpointStatus checks the asynchronous pieces behind one live public
// hostname. Every failure stays local to its row in the console: an API or
// resolver hiccup must not turn the app's workload status into a 502.
func (r *Runtime) EndpointStatus(ctx context.Context, space, app, host string) rt.EndpointStatus {
	return rt.EndpointStatus{
		Certificate: *r.certificateStatus(ctx, space, app, host),
		DNS:         *r.dnsStatus(ctx, space, app, host),
	}
}

func (r *Runtime) certificateStatus(ctx context.Context, space, app, host string) *rt.CertificateStatus {
	switch r.opts.TLS.EffectiveMode() {
	case "cert-manager":
		return r.certManagerStatus(ctx, space, app)
	case "ca":
		return r.caStatus(ctx)
	default:
		return r.acmeStatus(ctx, host)
	}
}

type objectCondition struct {
	Status             string
	Reason             string
	Message            string
	ObservedGeneration int64
}

func conditionOf(object map[string]any, conditionType string) (objectCondition, bool) {
	conditions, found, err := unstructured.NestedSlice(object, "status", "conditions")
	if err != nil || !found {
		return objectCondition{}, false
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || stringField(condition, "type") != conditionType {
			continue
		}
		observed, _, _ := unstructured.NestedInt64(condition, "observedGeneration")
		return objectCondition{
			Status:             stringField(condition, "status"),
			Reason:             stringField(condition, "reason"),
			Message:            stringField(condition, "message"),
			ObservedGeneration: observed,
		}, true
	}
	return objectCondition{}, false
}

func stringField(object map[string]any, field string) string {
	value, _, _ := unstructured.NestedString(object, field)
	return value
}

func objectTime(object map[string]any, fields ...string) time.Time {
	value, found, _ := unstructured.NestedString(object, fields...)
	if !found {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func (r *Runtime) certManagerStatus(ctx context.Context, space, app string) *rt.CertificateStatus {
	status := &rt.CertificateStatus{
		State:  rt.EndpointIssuing,
		Mode:   "cert-manager",
		Issuer: r.opts.TLS.CertificateIssuerKind() + " " + r.opts.TLS.IssuerName,
	}
	if r.c == nil || r.c.Dynamic == nil {
		status.State = rt.EndpointUnknown
		status.Message = "The Kubernetes Certificate API is unavailable."
		return status
	}

	name := manifests.AppCertificateName(space, app)
	certificate, err := r.c.Dynamic.Resource(gvrFor["Certificate"]).
		Namespace(r.opts.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		status.Message = "Waiting for the app Certificate to be created."
		return status
	case err != nil:
		status.State = rt.EndpointUnknown
		status.Message = "Could not read the app Certificate: " + err.Error()
		return status
	}
	if err := r.c.requireOwnedObject("Certificate", r.opts.SystemNamespace, name,
		certificate.GetLabels(), manifests.Labels(space, app, manifests.RoleEdge)); err != nil {
		status.State = rt.EndpointError
		status.Message = err.Error()
		return status
	}

	status.ExpiresAt = objectTime(certificate.Object, "status", "notAfter")
	status.RenewalTime = objectTime(certificate.Object, "status", "renewalTime")
	ready, hasReady := conditionOf(certificate.Object, "Ready")
	issuing, hasIssuing := conditionOf(certificate.Object, "Issuing")
	failures, _, _ := unstructured.NestedInt64(certificate.Object, "status", "failedIssuanceAttempts")
	lastFailure := objectTime(certificate.Object, "status", "lastFailureTime")

	// A condition tied to an older spec must not make a just-changed hostname
	// look ready. Older cert-manager versions omitted observedGeneration, so
	// zero remains compatible rather than being treated as stale.
	stale := hasReady && ready.ObservedGeneration > 0 && ready.ObservedGeneration < certificate.GetGeneration()
	switch {
	case stale:
		status.Message = "Waiting for cert-manager to reconcile the current Certificate generation."
	case hasReady && strings.EqualFold(ready.Status, "True"):
		status.State = rt.EndpointReady
		status.Reason = ready.Reason
		status.Message = ready.Message
		if hasIssuing && strings.EqualFold(issuing.Status, "True") {
			status.Renewing = true
			status.Reason = issuing.Reason
			status.Message = issuing.Message
		}
	case failures > 0 || !lastFailure.IsZero() || failureReason(ready.Reason):
		status.State = rt.EndpointError
		status.Reason = ready.Reason
		status.Message = ready.Message
	case hasReady && strings.EqualFold(ready.Status, "Unknown"):
		status.State = rt.EndpointUnknown
		status.Reason = ready.Reason
		status.Message = ready.Message
	case hasReady:
		status.Reason = ready.Reason
		status.Message = ready.Message
	case hasIssuing:
		status.Reason = issuing.Reason
		status.Message = issuing.Message
	}

	if status.State == rt.EndpointReady && !status.ExpiresAt.IsZero() && !status.ExpiresAt.After(time.Now()) {
		status.State = rt.EndpointError
		status.Reason = "Expired"
		status.Message = "cert-manager reports an expired certificate."
	}
	if status.Message == "" {
		switch status.State {
		case rt.EndpointReady:
			status.Message = "cert-manager has issued the certificate."
		case rt.EndpointError:
			status.Message = "The latest certificate issuance failed."
		case rt.EndpointUnknown:
			status.Message = "cert-manager has not reported a current condition."
		default:
			status.Message = "Waiting for cert-manager to issue the certificate."
		}
	}
	return status
}

func failureReason(reason string) bool {
	reason = strings.ToLower(reason)
	return strings.Contains(reason, "fail") || strings.Contains(reason, "denied") ||
		strings.Contains(reason, "invalid") || strings.Contains(reason, "error")
}

func (r *Runtime) acmeStatus(ctx context.Context, host string) *rt.CertificateStatus {
	status := &rt.CertificateStatus{State: rt.EndpointIssuing, Mode: "acme"}
	if r.c == nil || r.c.Clientset == nil {
		status.State = rt.EndpointUnknown
		status.Message = "The gateway certificate cache is unavailable."
		return status
	}
	secret, err := r.c.Clientset.CoreV1().Secrets(r.opts.SystemNamespace).
		Get(ctx, manifests.GatewayCertsSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		status.Message = "Waiting for the gateway to issue and cache this ACME certificate."
		return status
	case err != nil:
		status.State = rt.EndpointUnknown
		status.Message = "Could not read the gateway certificate cache: " + err.Error()
		return status
	}
	if err := r.c.requireOwnedObject("Secret", r.opts.SystemNamespace, manifests.GatewayCertsSecret,
		secret.Labels, manifests.SystemLabels(manifests.GatewayAppName)); err != nil {
		status.State = rt.EndpointError
		status.Message = err.Error()
		return status
	}

	// autocert's normal ECDSA cache key is the hostname. Keep the RSA key as
	// a compatibility fallback; the Secret cache spells '+' as '.plus.'.
	data := secret.Data[strings.ToLower(strings.TrimSuffix(host, "."))]
	if len(data) == 0 {
		data = secret.Data[strings.ToLower(strings.TrimSuffix(host, "."))+".plus.rsa"]
	}
	if len(data) == 0 {
		status.Message = "Waiting for the gateway to issue and cache this ACME certificate; check donkey-gateway logs if it persists."
		return status
	}
	leaf, _, err := certificatePair(data, data)
	if err != nil {
		status.State = rt.EndpointError
		status.Message = "The cached ACME certificate is invalid: " + err.Error()
		return status
	}
	status.ExpiresAt = leaf.NotAfter
	if err := leaf.VerifyHostname(host); err != nil {
		status.State = rt.EndpointError
		status.Message = "The cached ACME certificate does not cover this hostname."
		return status
	}
	if now := time.Now(); now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		status.State = rt.EndpointError
		status.Message = "The cached ACME certificate is outside its validity period."
		return status
	}
	status.State = rt.EndpointReady
	status.Message = "The gateway has issued and cached this ACME certificate."
	return status
}

func (r *Runtime) caStatus(ctx context.Context) *rt.CertificateStatus {
	status := &rt.CertificateStatus{State: rt.EndpointIssuing, Mode: "ca", Issuer: "donkey built-in CA"}
	if r.c == nil || r.c.Clientset == nil {
		status.State = rt.EndpointUnknown
		status.Message = "The gateway CA is unavailable."
		return status
	}
	secret, err := r.c.Clientset.CoreV1().Secrets(r.opts.SystemNamespace).
		Get(ctx, manifests.GatewayCASecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		status.Message = "Waiting for the gateway to create its built-in CA."
		return status
	case err != nil:
		status.State = rt.EndpointUnknown
		status.Message = "Could not read the gateway CA: " + err.Error()
		return status
	}
	if err := r.c.requireOwnedObject("Secret", r.opts.SystemNamespace, manifests.GatewayCASecret,
		secret.Labels, manifests.SystemLabels(manifests.GatewayAppName)); err != nil {
		status.State = rt.EndpointError
		status.Message = err.Error()
		return status
	}
	certificate, privateKey, err := certificatePair(secret.Data["ca.crt"], secret.Data["ca.key"])
	if err != nil {
		status.State = rt.EndpointError
		status.Message = "The gateway CA certificate is invalid: " + err.Error()
		return status
	}
	if _, ok := privateKey.(*ecdsa.PrivateKey); !ok || !certificate.IsCA {
		status.State = rt.EndpointError
		status.Message = "The gateway CA does not contain the expected EC CA keypair."
		return status
	}
	if now := time.Now(); now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		status.State = rt.EndpointError
		status.Message = "The gateway's built-in CA is outside its validity period."
		return status
	}
	status.State = rt.EndpointReady
	status.Message = "The built-in CA is ready; the gateway mints this hostname's certificate during TLS handshakes. Clients must trust the CA."
	return status
}

func certificatePair(certificatePEM, keyPEM []byte) (*x509.Certificate, any, error) {
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	if len(pair.Certificate) == 0 {
		return nil, nil, fmt.Errorf("the keypair contains no certificate")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	return certificate, pair.PrivateKey, nil
}

func (r *Runtime) dnsStatus(ctx context.Context, space, app, host string) *rt.DNSStatus {
	status := &rt.DNSStatus{
		State:      rt.EndpointUnmanaged,
		RecordType: r.opts.ExternalDNS.EffectiveRecordType(),
		Expected:   normalizedValues(r.opts.ExternalDNS.Targets),
	}
	if !r.opts.ExternalDNS.Enabled() {
		status.Message = "external-dns management is not enabled for this installation."
		return status
	}
	if r.c == nil || r.c.Dynamic == nil {
		status.State = rt.EndpointUnknown
		status.Message = "The Kubernetes DNSEndpoint API is unavailable."
		return status
	}

	name := manifests.AppDNSName(space, app)
	endpoint, err := r.c.Dynamic.Resource(gvrFor["DNSEndpoint"]).
		Namespace(r.opts.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		status.State = rt.EndpointPropagating
		status.Message = "Waiting for the app DNSEndpoint to be created."
		return status
	case err != nil:
		status.State = rt.EndpointUnknown
		status.Message = "Could not read the app DNSEndpoint: " + err.Error()
		return status
	}
	if err := r.c.requireOwnedObject("DNSEndpoint", r.opts.SystemNamespace, name,
		endpoint.GetLabels(), manifests.Labels(space, app, manifests.RoleEdge)); err != nil {
		status.State = rt.EndpointError
		status.Message = err.Error()
		return status
	}
	observed, found, _ := unstructured.NestedInt64(endpoint.Object, "status", "observedGeneration")
	status.Observed = found && observed >= endpoint.GetGeneration()
	if err := expectedDNSEndpoint(endpoint.Object, host, status.RecordType, status.Expected); err != nil {
		status.State = rt.EndpointError
		status.Message = err.Error()
		return status
	}

	resolver := r.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resolved, err := resolveDNS(lookupCtx, resolver, host, status.RecordType)
	resolved = normalizedValues(resolved)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			status.State = rt.EndpointPropagating
			status.Message = fmt.Sprintf("No %s answer is visible to the control plane yet.", status.RecordType)
			return status
		}
		status.State = rt.EndpointUnknown
		status.Message = "DNS lookup from the control plane failed: " + err.Error()
		return status
	}
	if !slices.Equal(status.Expected, resolved) {
		status.State = rt.EndpointPropagating
		status.Message = fmt.Sprintf("Expected %s %s; the control plane currently resolves %s.",
			status.RecordType, printableValues(status.Expected), printableValues(resolved))
		return status
	}
	status.State = rt.EndpointReady
	if status.Observed {
		status.Message = fmt.Sprintf("external-dns observed the record and the control plane resolves %s to %s.",
			status.RecordType, printableValues(resolved))
	} else {
		status.Message = "DNS resolves correctly; the DNSEndpoint has not reported that external-dns observed its current generation."
	}
	return status
}

func expectedDNSEndpoint(object map[string]any, host, recordType string, targets []string) error {
	endpoints, found, err := unstructured.NestedSlice(object, "spec", "endpoints")
	if err != nil || !found {
		return fmt.Errorf("the app DNSEndpoint has no readable endpoints")
	}
	for _, item := range endpoints {
		endpoint, ok := item.(map[string]any)
		if !ok || normalizeDNSName(stringField(endpoint, "dnsName")) != normalizeDNSName(host) {
			continue
		}
		actualType := strings.ToUpper(stringField(endpoint, "recordType"))
		actualTargets, _, _ := unstructured.NestedStringSlice(endpoint, "targets")
		if actualType != recordType || !slices.Equal(normalizedValues(actualTargets), targets) {
			return fmt.Errorf("the app DNSEndpoint does not match the configured %s target %s", recordType, printableValues(targets))
		}
		return nil
	}
	return fmt.Errorf("the app DNSEndpoint does not contain %s", host)
}

func resolveDNS(ctx context.Context, resolver dnsResolver, host, recordType string) ([]string, error) {
	switch recordType {
	case "CNAME":
		name, err := resolver.LookupCNAME(ctx, host)
		if err != nil {
			return nil, err
		}
		return []string{normalizeDNSName(name)}, nil
	case "AAAA":
		ips, err := resolver.LookupIP(ctx, "ip6", host)
		return ipStrings(ips), err
	default:
		ips, err := resolver.LookupIP(ctx, "ip4", host)
		return ipStrings(ips), err
	}
}

func ipStrings(ips []net.IP) []string {
	values := make([]string, 0, len(ips))
	for _, ip := range ips {
		values = append(values, ip.String())
	}
	return values
}

func normalizeDNSName(value string) string {
	return strings.ToLower(strings.TrimSuffix(value, "."))
}

func normalizedValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if ip := net.ParseIP(value); ip != nil {
			value = ip.String()
		} else {
			value = normalizeDNSName(value)
		}
		if value != "" {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func printableValues(values []string) string {
	if len(values) == 0 {
		return "no answer"
	}
	return strings.Join(values, ", ")
}
