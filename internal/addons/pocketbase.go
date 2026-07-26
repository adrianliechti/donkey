package addons

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	pbPort = 8090
	// PocketBase requires a syntactically valid email for its superuser; the
	// address is never mailed, it is only the login name of the account.
	pbAdminEmail = "admin@donkey.local"
)

// pocketbase publishes no official container image; this community image
// repacks the upstream release binaries and mirrors its version tags.
const pbImage = "ghcr.io/muchobien/pocketbase"

type pocketbase struct{}

func (pocketbase) Type() string { return "pocketbase" }
func (pocketbase) Description() string {
	return "Accounts, file uploads and a records API in one small server."
}
func (pocketbase) CredentialKey() string { return "POCKETBASE_ADMIN_PASSWORD" }

// DefaultVersion tracks a minor release line, so provisioned instances pick
// up patch releases on restart without jumping minors unasked.
func (pocketbase) DefaultVersion() string { return "0.39" }
func (pocketbase) DefaultStorage() string { return "1Gi" }

func (pocketbase) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "pocketbase" — the same way they reach each other.
	host := fmt.Sprintf("%s.%s.svc", in.Name, in.Namespace)
	return map[string]string{
		"POCKETBASE_URL":            fmt.Sprintf("http://%s:%d", host, pbPort),
		"POCKETBASE_ADMIN_EMAIL":    pbAdminEmail,
		"POCKETBASE_ADMIN_PASSWORD": in.Password,
	}
}

func (p pocketbase) Workload(in Input) (*Workload, error) {
	return &Workload{
		Image:    pbImage + ":" + in.Version,
		Port:     pbPort,
		PortName: "http",
		// The image's entrypoint creates (or updates) the superuser from
		// these before serving with /pb_data as its data dir.
		SecretEnv: map[string]string{
			"PB_ADMIN_EMAIL":    "POCKETBASE_ADMIN_EMAIL",
			"PB_ADMIN_PASSWORD": "POCKETBASE_ADMIN_PASSWORD",
		},
		DataPath: "/pb_data",
		// The image defaults to root; an ordinary user with the volume
		// group-owned is all PocketBase needs.
		RunAsUser:     1000,
		ReadyHTTPPath: "/api/health",
	}, nil
}

func (pocketbase) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("that doesn't look like a PocketBase address — expected https://pocketbase.example.com")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the PocketBase address names no host — expected https://pocketbase.example.com")
	}
	return map[string]string{
		"POCKETBASE_URL": strings.TrimRight(raw, "/"),
	}, nil
}
