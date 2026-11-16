package manifests

import (
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
)

const (
	// SystemCertificateName is the cert-manager Certificate used by the
	// gateway for the console and central OAuth callback.
	SystemCertificateName       = GatewayAppName
	SystemCertificateSecretName = GatewayAppName + "-tls"
	SystemDNSName               = GatewayAppName + "-dns"
)

// AppCertificateName includes the space because all certificates live in the
// system namespace. Space names cannot contain the separator.
func AppCertificateName(space, app string) string { return space + NameSeparator + app }

func AppCertificateSecretName(space, app string) string {
	return AppCertificateName(space, app) + "-tls"
}

func AppDNSName(space, app string) string { return AppCertificateName(space, app) + "-dns" }

// AppTrafficObjects returns the external hostname's certificate and optional
// DNS record. Routing itself remains a native donkey route ConfigMap.
func AppTrafficObjects(in Input) []runtime.Object {
	if in.Domain == "" {
		return nil
	}
	labels := Labels(in.Space, in.App.Name, RoleEdge)
	var objects []runtime.Object
	if in.TLS.UsesCertManager() {
		objects = append(objects, managedCertificate(
			AppCertificateName(in.Space, in.App.Name), in.SystemNamespace,
			AppCertificateSecretName(in.Space, in.App.Name), []string{in.Domain}, labels,
			in.TLS,
		))
	}
	if in.ExternalDNS.Enabled() {
		objects = append(objects, dnsEndpoint(
			AppDNSName(in.Space, in.App.Name), in.SystemNamespace,
			[]string{in.Domain}, labels, in.ExternalDNS,
		))
	}
	return objects
}

// systemTrafficObjects covers the base domain (the OAuth callback), a
// distinct server hostname and, where the installation has machines, the one
// address they all answer on — which is why starting a machine waits for
// neither a certificate nor a DNS record: both were made here, once. One SAN
// Certificate is enough; DNS keeps exact records because the apex can never
// be supplied by a wildcard record.
func systemTrafficObjects(in SystemInput) []runtime.Object {
	hosts := compactHosts(in.Domain, in.ServerHost(), in.MachineHost())
	if len(hosts) == 0 {
		return nil
	}
	labels := SystemLabels(GatewayAppName)
	var objects []runtime.Object
	if in.TLS.UsesCertManager() {
		objects = append(objects, managedCertificate(
			SystemCertificateName, in.Namespace, SystemCertificateSecretName, hosts, labels,
			in.TLS,
		))
	}
	if in.ExternalDNS.Enabled() {
		objects = append(objects, dnsEndpoint(SystemDNSName, in.Namespace, hosts, labels, in.ExternalDNS))
	}
	return objects
}

func compactHosts(hosts ...string) []string {
	var out []string
	for _, host := range hosts {
		if host != "" && !slices.Contains(out, host) {
			out = append(out, host)
		}
	}
	return out
}

func managedCertificate(name, namespace, secretName string, dnsNames []string, objectLabels map[string]string,
	tlsConfig config.InstallTLS,
) *unstructured.Unstructured {
	names := make([]any, 0, len(dnsNames))
	for _, host := range dnsNames {
		if host != "" {
			names = append(names, host)
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    toAnyMap(objectLabels),
		},
		"spec": map[string]any{
			"secretName": secretName,
			"secretTemplate": map[string]any{
				"labels": toAnyMap(objectLabels),
			},
			// Key type, lifetime, renewal window and usages remain issuer policy.
			"dnsNames": names,
			"issuerRef": map[string]any{
				"name":  tlsConfig.IssuerName,
				"kind":  tlsConfig.CertificateIssuerKind(),
				"group": tlsConfig.CertificateIssuerGroup(),
			},
		},
	}}
}

func dnsEndpoint(name, namespace string, hosts []string, objectLabels map[string]string, cfg config.InstallExternalDNS) *unstructured.Unstructured {
	targets := make([]any, 0, len(cfg.Targets))
	for _, target := range cfg.Targets {
		targets = append(targets, target)
	}
	endpoints := make([]any, 0, len(hosts))
	for _, host := range hosts {
		endpoint := map[string]any{
			"dnsName":    host,
			"recordType": cfg.EffectiveRecordType(),
			"targets":    targets,
		}
		if cfg.TTL > 0 {
			endpoint["recordTTL"] = cfg.TTL
		}
		endpoints = append(endpoints, endpoint)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "externaldns.k8s.io/v1alpha1",
		"kind":       "DNSEndpoint",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    toAnyMap(objectLabels),
		},
		"spec": map[string]any{"endpoints": endpoints},
	}}
}

func toAnyMap(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
