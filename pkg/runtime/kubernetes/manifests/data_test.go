package manifests

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

func dataObjects(t *testing.T, data config.InstallData) (*appsv1.Deployment, *corev1.PersistentVolumeClaim) {
	t.Helper()
	in := systemInput()
	in.Data = data
	var server *appsv1.Deployment
	var claim *corev1.PersistentVolumeClaim
	for _, obj := range SystemObjects(in) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name == ServerName {
				server = o
			}
		case *corev1.PersistentVolumeClaim:
			if o.Name == DataPVCName {
				claim = o
			}
		}
	}
	if server == nil {
		t.Fatal("no donkey-server Deployment")
	}
	return server, claim
}

func dataEnv(d *appsv1.Deployment) string {
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "DONKEY_DATA_DIR" {
			return e.Value
		}
	}
	return ""
}

func dataVolumeClaim(d *appsv1.Deployment) string {
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "data" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// oldPodFirst is a rolling update that never runs two pods: never Recreate,
// which an existing Deployment cannot switch to in an apply.
func oldPodFirst(d *appsv1.Deployment) bool {
	s := d.Spec.Strategy
	return s.Type == appsv1.RollingUpdateDeploymentStrategyType && s.RollingUpdate != nil &&
		s.RollingUpdate.MaxSurge.IntValue() == 0 && s.RollingUpdate.MaxUnavailable.IntValue() == 1
}

func TestServerDataVolume(t *testing.T) {
	// By default: donkey's own single-node claim, mounted, and the server
	// replaced rather than rolled so a new pod never waits on the old one's
	// volume.
	server, claim := dataObjects(t, config.InstallData{})
	if claim == nil || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce || claim.Spec.Resources.Requests.Storage().String() != config.DefaultDataSize {
		t.Fatalf("default claim = %+v", claim)
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatal("no storage class means the cluster default")
	}
	if dataEnv(server) != DataMountPath || dataVolumeClaim(server) != DataPVCName {
		t.Fatal("the server must mount the claim and be told where")
	}
	if !oldPodFirst(server) {
		t.Fatalf("a ReadWriteOnce volume needs the old pod gone first, got %+v", server.Spec.Strategy)
	}
	if fs := server.Spec.Template.Spec.SecurityContext.FSGroup; fs == nil || *fs != systemRunAsUser {
		t.Fatal("the volume must be writable for the server's uid")
	}

	// Shared storage (NFS in production): a claim of the installation's own,
	// and rolling updates stay.
	server, claim = dataObjects(t, config.InstallData{ExistingClaim: "donkey-data-nfs", AccessMode: config.ReadWriteMany})
	if claim != nil {
		t.Fatal("an existing claim is not created again")
	}
	if dataVolumeClaim(server) != "donkey-data-nfs" {
		t.Fatalf("server mounts %q", dataVolumeClaim(server))
	}
	if oldPodFirst(server) {
		t.Fatal("a ReadWriteMany volume keeps the surging rolling update")
	}

	_, claim = dataObjects(t, config.InstallData{StorageClass: "nfs-client", Size: "20Gi", AccessMode: config.ReadWriteMany})
	if claim == nil || *claim.Spec.StorageClassName != "nfs-client" || claim.Spec.AccessModes[0] != corev1.ReadWriteMany ||
		claim.Spec.Resources.Requests.Storage().String() != "20Gi" {
		t.Fatalf("shaped claim = %+v", claim)
	}

	server, claim = dataObjects(t, config.InstallData{Disabled: true})
	if claim != nil || dataEnv(server) != "" || dataVolumeClaim(server) != "" {
		t.Fatal("a disabled data volume is not mounted")
	}
	if oldPodFirst(server) {
		t.Fatal("without a volume the server rolls as before")
	}
}
