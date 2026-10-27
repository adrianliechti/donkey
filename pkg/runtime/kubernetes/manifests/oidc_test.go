package manifests

import (
	"reflect"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestOIDCRegistrationRouteRoundTrip(t *testing.T) {
	in := testInput(t)
	in.App.OIDC = &config.OIDCClient{RedirectURIs: []string{"https://shop.acme.apps.example.com/callback"}}
	cm := appRoute(in)
	route, err := ReadRoute(cm)
	if err != nil || !reflect.DeepEqual(route.OIDC, in.App.OIDC) {
		t.Fatalf("registration lost in route: %+v, %v", route.OIDC, err)
	}
	path := routeSettings(in, in.Domain, "/api")
	if path.OIDC != nil {
		t.Fatal("shared route registered a duplicate client")
	}
}

func TestGatewayAuthStateUsesPersistentVolume(t *testing.T) {
	in := SystemInput{Namespace: "system", Data: config.InstallData{StorageClass: "fast"}}
	claim := donkeyGatewayDataClaim(in)
	if claim.Name != GatewayDataPVC || claim.Spec.Resources.Requests.Storage().String() != "1Gi" || claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast" || !reflect.DeepEqual(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Fatalf("bad gateway claim: %+v", claim)
	}
	deployment := donkeyGatewayDeployment(in)
	if *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("local gateway state requires one writer and Recreate upgrades")
	}
	pod := deployment.Spec.Template.Spec
	if pod.SecurityContext.FSGroup == nil {
		t.Fatal("gateway volume is not writable by its non-root user")
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != GatewayDataPVC {
		t.Fatal("gateway state volume missing")
	}
	if mounts := pod.Containers[0].VolumeMounts; len(mounts) != 1 || mounts[0].MountPath != "/var/lib/donkey" {
		t.Fatal("gateway state mount missing")
	}
}
