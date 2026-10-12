package addons

import (
	"context"
	"fmt"
	"strings"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

const (
	rfPort = 9000
	// RustFS accepts any string as its root access key; a fixed one keeps the
	// binding predictable — the secret key is the credential that matters.
	rfAccessKey = "donkey"
	// RustFS follows the S3 default region; pinning it on both sides (server
	// env and binding) keeps SDK signing from ever guessing.
	rfRegion = "us-east-1"
)

// rustfs publishes an official image; it runs as its own user (uid 10001)
// and serves the S3 API on 9000 with an optional console on 9001, which
// donkey keeps switched off — apps talk S3, humans get the console dialog.
const rfImage = "rustfs/rustfs"

type rustfs struct{}

func (rustfs) Type() string          { return "s3" }
func (rustfs) CredentialKey() string { return "AWS_SECRET_ACCESS_KEY" }

// The default version pins an exact release: upstream is still in its
// prerelease line, where patch jumps can carry breaking changes a data store
// must not pick up on a restart.
func (r rustfs) Describe(context.Context) (*addonv1.Offering, error) {
	return description(r, "S3", "S3-compatible object storage — buckets and files, in one small server.",
		CategoryStorage, ProtocolS3, ObjectSchema(map[string]any{
			ParamVersion: VersionParameter("1.0.0-rc.3", "A tag of the rustfs/rustfs image; exact releases only, prereleases may break on upgrade."),
			ParamStorage: StorageParameter("1Gi"),
		}),
		"AWS_SECRET_ACCESS_KEY"), nil
}

func (rustfs) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "s3" — the same way they reach each other.
	endpoint := fmt.Sprintf("http://%s.%s.svc:%d", in.HostName(), in.Namespace, rfPort)
	return map[string]string{
		// The AWS SDK convention: every S3 client picks these up without a
		// line of configuration.
		"AWS_ACCESS_KEY_ID":     rfAccessKey,
		"AWS_SECRET_ACCESS_KEY": in.Password,
		"AWS_ENDPOINT_URL_S3":   endpoint,
		"AWS_REGION":            rfRegion,
		// The same endpoint once more, for apps that read a single var.
		"S3_URL": endpoint,
	}
}

func (r rustfs) Workload(in Input) (*Workload, error) {
	return &Workload{
		Image:    in.ImageRef(rfImage),
		Port:     rfPort,
		PortName: "s3",
		Env: map[string]string{
			// One volume is the single-node layout; RustFS initializes it
			// on first start.
			"RUSTFS_VOLUMES": "/data",
			"RUSTFS_REGION":  rfRegion,
			// The web console would be a second, unmanaged door into the
			// data — apps speak S3, admins get donkey's own dialog.
			"RUSTFS_CONSOLE_ENABLE": "false",
		},
		SecretEnv: map[string]string{
			"RUSTFS_ACCESS_KEY": "AWS_ACCESS_KEY_ID",
			"RUSTFS_SECRET_KEY": "AWS_SECRET_ACCESS_KEY",
		},
		DataPath: "/data",
		// The image ships its own unprivileged user (rustfs, 10001).
		RunAsUser:     10001,
		ReadyHTTPPath: "/health",
	}, nil
}

// ExternalBinding derives the endpoint half of the contract from an S3
// address. Credentials don't travel in a bare URL — the app keeps whatever
// keys it already has.
func (rustfs) ExternalBinding(raw string) (map[string]string, error) {
	endpoint := strings.TrimRight(raw, "/")
	binding := map[string]string{"AWS_ENDPOINT_URL_S3": endpoint, "S3_URL": endpoint}
	if err := ValidateBinding(ProtocolS3, binding); err != nil {
		return nil, fmt.Errorf("expected an S3 http or https endpoint without credentials, path, query or fragment")
	}
	return binding, nil
}
