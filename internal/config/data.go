package config

import (
	"fmt"
	"regexp"
)

// InstallData is donkey-server's own data volume. Today it holds every app's
// source history (one git repository per app, under source/); it is a
// general volume so later server-side data has a place too. On by default;
// production installations usually put it on shared storage (an NFS-backed
// claim).
type InstallData struct {
	// Disabled gives donkey-server no data volume, and with it no source
	// history.
	Disabled bool `json:"disabled,omitempty"`

	// ExistingClaim (Kubernetes) names a PersistentVolumeClaim you manage in
	// the system namespace. Without it donkey creates donkey-data.
	ExistingClaim string `json:"existingClaim,omitempty"`
	// StorageClass and Size (Kubernetes) shape the claim donkey creates;
	// empty means the cluster's default class and DefaultDataSize.
	StorageClass string `json:"storageClass,omitempty"`
	Size         string `json:"size,omitempty"`
	// AccessMode (Kubernetes) is the claim's: ReadWriteOnce (the default) or
	// ReadWriteMany. Only a shared volume lets a new donkey-server start
	// before the old one stops, so with ReadWriteOnce the server is replaced
	// rather than rolled.
	AccessMode string `json:"accessMode,omitempty"`
}

const (
	DefaultDataSize = "5Gi"
	ReadWriteOnce   = "ReadWriteOnce"
	ReadWriteMany   = "ReadWriteMany"
)

var quantityRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`)

// Enabled reports whether donkey-server gets a data volume.
func (d InstallData) Enabled() bool { return !d.Disabled }

// Shared reports whether several donkey-server pods can mount the volume at
// once, wherever they run.
func (d InstallData) Shared() bool { return d.AccessMode == ReadWriteMany }

// ClaimSize is the size of the claim donkey creates.
func (d InstallData) ClaimSize() string {
	if d.Size != "" {
		return d.Size
	}
	return DefaultDataSize
}

// Validate checks what can be wrong without consulting the runtime.
func (d InstallData) Validate() error {
	if d.Disabled && (d.ExistingClaim != "" || d.StorageClass != "" || d.Size != "" || d.AccessMode != "") {
		return fmt.Errorf("data.disabled gives donkey-server no volume, so the other data settings would do nothing — remove them or the disabled flag")
	}
	switch d.AccessMode {
	case "", ReadWriteOnce, ReadWriteMany:
	default:
		return fmt.Errorf("data.accessMode must be %s or %s, not %q", ReadWriteOnce, ReadWriteMany, d.AccessMode)
	}
	if d.Size != "" && !quantityRE.MatchString(d.Size) {
		return fmt.Errorf("data.size %q is not a storage size (like 5Gi)", d.Size)
	}
	if d.ExistingClaim != "" {
		if !ValidNamespace(d.ExistingClaim) {
			return fmt.Errorf("data.existingClaim %q is not a valid claim name", d.ExistingClaim)
		}
		// The claim is yours: its class and size were settled when you made it.
		if d.StorageClass != "" || d.Size != "" {
			return fmt.Errorf("data.storageClass and data.size shape the claim donkey creates — with data.existingClaim they would do nothing")
		}
	}
	return nil
}

// ValidateRuntime rejects settings that belong to the other runtime, so a
// setting never quietly does nothing. On Docker the data lives in the
// donkey-data volume the server already has; there is nothing to shape.
func (d InstallData) ValidateRuntime(runtime string) error {
	if runtime == "docker" && (d.ExistingClaim != "" || d.StorageClass != "" || d.Size != "" || d.AccessMode != "") {
		return fmt.Errorf("data.existingClaim, storageClass, size and accessMode are for Kubernetes; the Docker runtime keeps the data in its donkey-data volume")
	}
	return nil
}
