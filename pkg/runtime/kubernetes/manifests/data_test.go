package manifests

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

// dataObjects renders the control plane and picks out what the data volume
// touches: both Deployments and donkey's own claim.
func dataObjects(t *testing.T, data config.InstallData) (server, orchestrator *appsv1.Deployment, claim *corev1.PersistentVolumeClaim) {
	t.Helper()
	in := systemInput()
	in.Data = data
	for _, obj := range SystemObjects(in) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			switch o.Name {
			case ServerName:
				server = o
			case OrchestratorName:
				orchestrator = o
			}
		case *corev1.PersistentVolumeClaim:
			if o.Name == DataPVCName {
				claim = o
			}
		}
	}
	if server == nil || orchestrator == nil {
		t.Fatal("no donkey-server or donkey-orchestrator Deployment")
	}
	return server, orchestrator, claim
}

func dataEnv(d *appsv1.Deployment) string {
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "DONKEY_DATA_DIR" {
			return e.Value
		}
	}
	return ""
}

func mountedClaim(d *appsv1.Deployment) string {
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "data" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

func TestDataVolume(t *testing.T) {
	// By default: donkey's own claim, mounted into the orchestrator alone —
	// it writes the history and serves every read of it.
	server, orchestrator, claim := dataObjects(t, config.InstallData{})
	if claim == nil || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce || claim.Spec.Resources.Requests.Storage().String() != config.DefaultDataSize {
		t.Fatalf("default claim = %+v", claim)
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatal("no storage class means the cluster default")
	}
	if dataEnv(orchestrator) != DataMountPath || mountedClaim(orchestrator) != DataPVCName {
		t.Fatal("the orchestrator must mount the claim and be told where")
	}
	if fs := orchestrator.Spec.Template.Spec.SecurityContext.FSGroup; fs == nil || *fs != systemRunAsUser {
		t.Fatal("the volume must be writable for the orchestrator's uid")
	}
	// One pod at a time may hold a single-node volume: the orchestrator is
	// replaced, never surged.
	if orchestrator.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("orchestrator strategy = %q", orchestrator.Spec.Strategy.Type)
	}
	if dataEnv(server) != "" || mountedClaim(server) != "" {
		t.Fatal("donkey-server stays stateless: it forwards history reads, it mounts nothing")
	}

	// Shared storage (NFS in production): a claim of the installation's own.
	_, orchestrator, claim = dataObjects(t, config.InstallData{ExistingClaim: "donkey-data-nfs"})
	if claim != nil {
		t.Fatal("an existing claim is not created again")
	}
	if mountedClaim(orchestrator) != "donkey-data-nfs" {
		t.Fatalf("orchestrator mounts %q", mountedClaim(orchestrator))
	}

	_, _, claim = dataObjects(t, config.InstallData{StorageClass: "nfs-client", Size: "20Gi"})
	if claim == nil || *claim.Spec.StorageClassName != "nfs-client" || claim.Spec.Resources.Requests.Storage().String() != "20Gi" {
		t.Fatalf("shaped claim = %+v", claim)
	}
}
