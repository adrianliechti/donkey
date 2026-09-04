package manifests

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
)

func kindsOf(objects []runtime.Object) map[string]int {
	kinds := map[string]int{}
	for _, object := range objects {
		kinds[object.GetObjectKind().GroupVersionKind().Kind]++
	}
	return kinds
}

func TestAppCertificateAndDNSEndpoint(t *testing.T) {
	in := testInput(t)
	in.TLS = config.InstallTLS{
		Mode:       "cert-manager",
		IssuerName: "platform-ca",
		IssuerKind: "ClusterIssuer", IssuerGroup: "cert-manager.io",
	}
	in.ExternalDNS = config.InstallExternalDNS{
		Targets: []string{"10.204.166.5"}, TTL: 300,
	}

	objects := NetworkObjects(in)
	if got, want := kindsOf(objects), map[string]int{"ConfigMap": 1, "Certificate": 1, "DNSEndpoint": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected network objects: %v", got)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: cert-manager.io/v1",
		"kind: Certificate",
		"name: donkey-app-acme.shop",
		"namespace: donkey-system",
		"secretName: donkey-app-acme.shop-tls",
		"name: platform-ca",
		"kind: ClusterIssuer",
		"group: cert-manager.io",
		"apiVersion: externaldns.k8s.io/v1alpha1",
		"kind: DNSEndpoint",
		"dnsName: shop.acme.apps.example.com",
		"recordType: A",
		"recordTTL: 300",
		"- 10.204.166.5",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("app traffic objects should contain %q:\n%s", want, rendered)
		}
	}
}

func TestCertificateNamesIncludeLogicalSpace(t *testing.T) {
	if AppCertificateName("alpha", "shop") == AppCertificateName("beta", "shop") {
		t.Fatal("two spaces produced the same certificate name")
	}
	if AppCertificateName("a", "b-c") == AppCertificateName("a-b", "c") {
		t.Fatal("certificate name is ambiguous across space/app pairs")
	}
}

func TestAppTrafficObjectsAreOptionalAndInternalSafe(t *testing.T) {
	in := testInput(t)
	objects := NetworkObjects(in)
	if got, want := kindsOf(objects), map[string]int{"ConfigMap": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ACME/CA mode must not render managed traffic objects: %v", got)
	}

	in.TLS = config.InstallTLS{Mode: "cert-manager", IssuerName: "platform-ca"}
	in.ExternalDNS = config.InstallExternalDNS{Targets: []string{"10.204.166.5"}, RecordType: "A"}
	in.Domain = ""
	objects = NetworkObjects(in)
	if got := kindsOf(objects); len(got) != 0 {
		t.Fatalf("an internal app must have no public traffic objects: %v", got)
	}
}

func TestCertificateIssuerCanBeExternal(t *testing.T) {
	in := testInput(t)
	in.TLS = config.InstallTLS{
		Mode:       "cert-manager",
		IssuerName: "vault-pki",
		IssuerKind: "VaultClusterIssuer", IssuerGroup: "pki.example.com",
	}
	rendered, err := Render(AppTrafficObjects(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: VaultClusterIssuer", "group: pki.example.com"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("external issuer setting %q did not survive:\n%s", want, rendered)
		}
	}
}

func TestSystemCertificateCoversCallbackAndConsole(t *testing.T) {
	in := systemInput()
	in.ServerHostname = "console.apps.example.com"
	in.TLS = config.InstallTLS{Mode: "cert-manager", IssuerName: "platform-ca"}
	in.ExternalDNS = config.InstallExternalDNS{Targets: []string{"10.204.166.5"}, RecordType: "A"}
	objects := systemTrafficObjects(in)
	if got, want := kindsOf(objects), map[string]int{"Certificate": 1, "DNSEndpoint": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected system traffic objects: %v", got)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"apps.example.com", "console.apps.example.com"} {
		if !strings.Contains(rendered, host) {
			t.Errorf("system certificate and DNS should contain %q", host)
		}
	}
}
