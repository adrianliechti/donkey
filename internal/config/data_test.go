package config

import (
	"strings"
	"testing"
)

func TestInstallDataValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    InstallData
		runtime string
		err     string
	}{
		{name: "default", runtime: "kubernetes"},
		{name: "nfs claim", data: InstallData{ExistingClaim: "donkey-data-nfs", AccessMode: ReadWriteMany}, runtime: "kubernetes"},
		{name: "shaped claim", data: InstallData{StorageClass: "nfs-client", Size: "20Gi"}, runtime: "kubernetes"},
		{name: "docker default", runtime: "docker"},
		{name: "disabled", data: InstallData{Disabled: true}, runtime: "docker"},
		{name: "disabled with settings", data: InstallData{Disabled: true, Size: "5Gi"}, runtime: "kubernetes", err: "would do nothing"},
		{name: "bad access mode", data: InstallData{AccessMode: "ReadOnlyMany"}, runtime: "kubernetes", err: "accessMode"},
		{name: "bad size", data: InstallData{Size: "lots"}, runtime: "kubernetes", err: "not a storage size"},
		{name: "claim and size", data: InstallData{ExistingClaim: "mine", Size: "5Gi"}, runtime: "kubernetes", err: "would do nothing"},
		{name: "bad claim", data: InstallData{ExistingClaim: "Not_A_Name"}, runtime: "kubernetes", err: "not a valid claim name"},
		{name: "claim on docker", data: InstallData{ExistingClaim: "mine"}, runtime: "docker", err: "for Kubernetes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.data.Validate()
			if err == nil {
				err = tc.data.ValidateRuntime(tc.runtime)
			}
			if tc.err == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("error = %v, want one about %q", err, tc.err)
			}
		})
	}
}
