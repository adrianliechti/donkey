package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	runtimedocker "github.com/adrianliechti/donkey/pkg/runtime/docker"
)

func TestDockerInstallerRequiresExactSystemIdentity(t *testing.T) {
	if err := requireDockerSystemResource("container", dockerServerName,
		runtimedocker.SystemLabels("server"), "server"); err != nil {
		t.Fatal(err)
	}
	if err := requireDockerSystemResource("container", dockerServerName,
		runtimedocker.SystemLabels("gateway"), "server"); err == nil {
		t.Fatal("a different control-plane role was accepted")
	}
	labels := runtimedocker.SystemLabels("server")
	labels["local.donkey.space"] = "team"
	if err := requireDockerSystemResource("container", dockerServerName, labels, "server"); err == nil {
		t.Fatal("a tenant resource was accepted as control-plane infrastructure")
	}
}

func TestValidateRegistryCASecretDataRequiresCACrtPEM(t *testing.T) {
	if err := validateRegistryCASecretData("enterprise-ca", nil); err == nil {
		t.Fatal("a missing CA Secret was accepted")
	}
	if err := validateRegistryCASecretData("enterprise-ca", map[string]string{"tls.crt": "not the requested key"}); err == nil {
		t.Fatal("a Secret without ca.crt was accepted")
	}
	if err := validateRegistryCASecretData("enterprise-ca", map[string]string{"ca.crt": "not pem"}); err == nil {
		t.Fatal("invalid PEM was accepted")
	}
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	valid := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err := validateRegistryCASecretData("enterprise-ca", map[string]string{"ca.crt": valid}); err != nil {
		t.Fatalf("PEM certificate was rejected: %v", err)
	}
	if fingerprintRegistryCA(valid) == fingerprintRegistryCA(valid+"\n") {
		t.Fatal("different CA Secret contents produced the same rollout fingerprint")
	}
}

func TestRegistryPullAuthUsesEnterpriseHostWithPrefixedURL(t *testing.T) {
	reg := config.Registry{URL: "registry.example.com/platform/donkey", Username: "builder"}
	encoded, err := registryPullAuth(reg, "builder", "s3cret", "registry.example.com/mirror/moby/buildkit:v1")
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("same-host mirrored image should receive registry auth")
	}
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var auth map[string]string
	if err := json.Unmarshal(raw, &auth); err != nil {
		t.Fatal(err)
	}
	if auth["username"] != "builder" || auth["password"] != "s3cret" || auth["serveraddress"] != "registry.example.com" {
		t.Fatalf("wrong registry auth: %v", auth)
	}

	public, err := registryPullAuth(reg, "builder", "s3cret", "docker.io/moby/buildkit:v1")
	if err != nil {
		t.Fatal(err)
	}
	if public != "" {
		t.Fatal("enterprise credentials must not be sent to a public registry")
	}
}

func TestBuilderEnvMapIncludesInstallationInstructions(t *testing.T) {
	got := builderEnvMap(config.BuildSettings{Instructions: "Use approved base-image prefixes."})
	if got["DONKEY_BUILDER_INSTRUCTIONS"] != "Use approved base-image prefixes." {
		t.Fatalf("builder instructions missing from Docker server env: %v", got)
	}
}

func TestValidateDockerConfigSecretDataUsesConfiguredSourceHost(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("reader:token"))
	data := map[string]string{
		".dockerconfigjson": `{"auths":{"oci.mirror.example.com":{"auth":"` + auth + `"}}}`,
	}
	if err := validateDockerConfigSecretData("build.packages.dockerConfigSecret", "package-auth", data, "oci.mirror.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerConfigSecretData("build.packages.dockerConfigSecret", "package-auth", data, "packages.example.com"); err == nil {
		t.Fatal("a Secret missing the configured Docker source host was accepted")
	}
	emptyUsername := base64.StdEncoding.EncodeToString([]byte(":token"))
	incomplete := map[string]string{
		".dockerconfigjson": `{"auths":{"oci.mirror.example.com":{"auth":"` + emptyUsername + `"}}}`,
	}
	if err := validateDockerConfigSecretData("build.packages.dockerConfigSecret", "package-auth", incomplete, "oci.mirror.example.com"); err == nil {
		t.Fatal("an incomplete Docker config credential was accepted")
	}
	if err := validateDockerConfigSecretData("registry.dockerConfigSecret", "missing", nil, ""); err == nil {
		t.Fatal("a missing Docker config Secret was accepted")
	}
}
