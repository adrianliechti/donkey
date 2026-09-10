package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestPackagesAreAlwaysRenderedWithPublicDefaults(t *testing.T) {
	out := componentNames(systemInput())
	for _, want := range []string{
		"name: donkey-packages",
		"DONKEY_PACKAGES_NPM_URL", config.DefaultNPMUpstream,
		"DONKEY_PACKAGES_PYPI_URL", config.DefaultPyPIUpstream,
		"DONKEY_PACKAGES_GO_URL", config.DefaultGoUpstream,
		"DONKEY_PACKAGES_MAVEN_URL", config.DefaultMavenUpstream,
		"DONKEY_PACKAGES_NUGET_URL", config.DefaultNuGetUpstream,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("always-on package proxy is missing %q", want)
		}
	}
	service := packagesObjects(systemInput())[1].(*corev1.Service)
	if got := service.Spec.Ports[0].Port; got != 80 {
		t.Errorf("package service port = %d, want 80", got)
	}
	if got := service.Spec.Ports[0].TargetPort.IntVal; got != packagesPort {
		t.Errorf("package target port = %d, want %d", got, packagesPort)
	}
}

func TestPackageCredentialsStayInSecretAndAreHostScoped(t *testing.T) {
	in := systemInput()
	in.Build.Packages = config.InstallPackages{
		NPM: "https://npm.artifactory.example.com/npm", PyPI: "https://pypi.artifactory.example.com/pypi", Go: "https://go.artifactory.example.com/go",
		Maven: "https://maven.artifactory.example.com/maven", NuGet: "https://nuget.artifactory.example.com/nuget/v3/index.json",
		CredentialHosts: []string{"npm.artifactory.example.com", "pypi.artifactory.example.com", "go.artifactory.example.com", "maven.artifactory.example.com", "nuget.artifactory.example.com"},
		Username:        "reader", Password: "super-secret-token",
	}
	dep := packagesDeployment(in)
	foundPassword := false
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name != "DONKEY_PACKAGES_PASSWORD" {
			continue
		}
		foundPassword = true
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Key != "DONKEY_PACKAGES_PASSWORD" {
			t.Errorf("package password is not a Secret reference: %+v", e)
		}
	}
	if !foundPassword {
		t.Fatal("credentialed package proxy has no password environment")
	}
	foundHosts := false
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "DONKEY_PACKAGES_CREDENTIAL_HOSTS" && strings.Contains(e.Value, "npm.artifactory.example.com") && strings.Contains(e.Value, "pypi.artifactory.example.com") {
			foundHosts = true
		}
	}
	if !foundHosts {
		t.Fatal("package credential hosts did not reach Deployment")
	}
	rendered, err := Render([]runtime.Object{dep})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "super-secret-token") {
		t.Fatal("package password leaked into Deployment")
	}
}

func TestPackageProxyOnlyAdmitsBuildkit(t *testing.T) {
	p := packagesNetworkPolicy(systemInput())
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 1 {
		t.Fatalf("package ingress = %+v", p.Spec.Ingress)
	}
	peer := p.Spec.Ingress[0].From[0]
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels[LabelRole] != SystemRole(buildkitName) {
		t.Fatalf("package peer = %+v, want only %s", peer, buildkitName)
	}
}

func TestPackageDockerConfigSecretIsMountedReadOnly(t *testing.T) {
	in := systemInput()
	in.Build.Packages = config.InstallPackages{
		NPM: "https://packages.example.com/npm", ResourceHosts: []string{"artifacts.example.com"},
		CredentialHosts:    []string{"packages.example.com", "artifacts.example.com"},
		DockerConfigSecret: "package-registry-auth",
		DockerConfigHost:   "oci.mirror.example.com",
	}
	dep := packagesDeployment(in)
	container := dep.Spec.Template.Spec.Containers[0]
	foundDir, foundHost, foundResources, foundMount := false, false, false, false
	for _, env := range container.Env {
		switch env.Name {
		case "DONKEY_PACKAGES_DOCKER_CONFIG_DIR":
			foundDir = env.Value == packagesDockerConfigPath
		case "DONKEY_PACKAGES_DOCKER_CONFIG_HOST":
			foundHost = env.Value == "oci.mirror.example.com"
		case "DONKEY_PACKAGES_RESOURCE_HOSTS":
			foundResources = env.Value == "artifacts.example.com"
		}
	}
	for _, mounted := range container.VolumeMounts {
		if mounted.Name == "package-auth" && mounted.MountPath == packagesDockerConfigPath && mounted.ReadOnly {
			foundMount = true
		}
	}
	if !foundDir || !foundHost || !foundResources || !foundMount {
		t.Fatalf("Docker config wiring: dir=%v host=%v resources=%v mount=%v", foundDir, foundHost, foundResources, foundMount)
	}
	for _, volume := range dep.Spec.Template.Spec.Volumes {
		if volume.Name == "package-auth" {
			if volume.Secret == nil || volume.Secret.SecretName != "package-registry-auth" {
				t.Fatalf("package-auth volume = %+v", volume)
			}
			return
		}
	}
	t.Fatal("package Docker config Secret has no volume")
}
