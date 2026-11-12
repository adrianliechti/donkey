package runtime

import (
	"context"

	"github.com/adrianliechti/donkey/internal/config"
	"golang.org/x/crypto/acme/autocert"
)

// GatewaySnapshot is a coherent route and membership view. Runtime object
// names, ownership labels and serialization stay in the runtime adapter.
type GatewaySnapshot struct {
	Routes   []config.GatewayRoute
	Spaces   map[string]config.SpaceConfig
	Platform config.PlatformConfig
}

type GatewayRouteSource interface {
	GatewaySnapshot(context.Context) (GatewaySnapshot, error)
}

type GatewayCertificate struct {
	Name                    string
	Certificate, PrivateKey []byte
}

type GatewayTLSStore interface {
	// PrepareGateway performs startup-only migration of existing TLS state.
	PrepareGateway(context.Context) error
	GatewayCertificateCache() autocert.Cache
	// GatewayCA returns the saved CA, atomically initializing it if absent.
	GatewayCA(context.Context, func() ([]byte, []byte, error)) ([]byte, []byte, error)
}

// GatewayCertificateSource is optional: runtimes with externally managed
// certificates implement it; the gateway rejects that TLS mode otherwise.
type GatewayCertificateSource interface {
	GatewayCertificates(context.Context) ([]GatewayCertificate, error)
}

// GatewayBackend is deliberately smaller than Provider: the edge needs a
// read-only route source and TLS persistence, not app deployment privileges.
type GatewayBackend interface {
	GatewayRouteSource
	GatewayTLSStore
}
