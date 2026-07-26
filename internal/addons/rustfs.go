package addons

import (
	"fmt"
	"net/url"
	"strings"
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

func (rustfs) Type() string { return "rustfs" }
func (rustfs) Description() string {
	return "S3-compatible object storage — buckets and files, in one small server."
}
func (rustfs) CredentialKey() string { return "AWS_SECRET_ACCESS_KEY" }

// DefaultVersion pins an exact release: upstream is still in its beta line,
// where patch jumps can carry breaking changes a data store must not pick
// up on a restart.
func (rustfs) DefaultVersion() string { return "1.0.0-beta.11" }
func (rustfs) DefaultStorage() string { return "1Gi" }

func (rustfs) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "rustfs" — the same way they reach each other.
	endpoint := fmt.Sprintf("http://%s.%s.svc:%d", in.Name, in.Namespace, rfPort)
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
		Image:    rfImage + ":" + in.Version,
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
// keys it already has, same as an external PocketBase keeps its login.
func (rustfs) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("that doesn't look like an S3 endpoint — expected https://s3.example.com")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the S3 endpoint names no host — expected https://s3.example.com")
	}
	// The trailing slash has to go: SDKs join bucket paths onto it.
	endpoint := strings.TrimRight(raw, "/")
	return map[string]string{
		"AWS_ENDPOINT_URL_S3": endpoint,
		"S3_URL":              endpoint,
	}, nil
}
