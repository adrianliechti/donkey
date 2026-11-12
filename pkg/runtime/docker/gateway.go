package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/storage"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"golang.org/x/crypto/acme/autocert"
)

type GatewayAdapter struct{ StateDir string }

func (g *GatewayAdapter) GatewaySnapshot(ctx context.Context) (rt.GatewaySnapshot, error) {
	routes, err := ReadRoutes(g.StateDir)
	if err != nil {
		return rt.GatewaySnapshot{}, err
	}
	spaces, platform, err := ReadGatewayPolicies(g.StateDir)
	return rt.GatewaySnapshot{Routes: routes, Spaces: spaces, Platform: platform}, err
}

func (g *GatewayAdapter) PrepareGateway(context.Context) error { return nil }

func (g *GatewayAdapter) GatewayCertificateCache() autocert.Cache {
	return autocert.DirCache(filepath.Join(g.StateDir, "certs"))
}

func (g *GatewayAdapter) GatewayCA(ctx context.Context, create func() ([]byte, []byte, error)) ([]byte, []byte, error) {
	dir := filepath.Join(g.StateDir, "ca")
	store := &storage.File{Path: filepath.Join(dir, "ca.json")}
	var pair rt.GatewayCertificate
	err := store.Update(ctx, func(raw []byte) ([]byte, error) {
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &pair); err != nil {
				return nil, err
			}
			if len(pair.Certificate) == 0 || len(pair.PrivateKey) == 0 {
				return nil, fmt.Errorf("incomplete stored CA")
			}
			return raw, nil
		}
		// Preserve the CA already trusted by installations using the old pair.
		cert, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
		if err == nil {
			key, err := os.ReadFile(filepath.Join(dir, "ca.key"))
			if err != nil {
				return nil, err
			}
			pair.Certificate, pair.PrivateKey = cert, key
		} else if errors.Is(err, os.ErrNotExist) {
			pair.Certificate, pair.PrivateKey, err = create()
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
		return json.Marshal(pair)
	})
	if err != nil {
		return nil, nil, err
	}
	// Keep the operator's existing CA export paths available.
	for name, data := range map[string][]byte{"ca.crt": pair.Certificate, "ca.key": pair.PrivateKey} {
		if err := (&storage.File{Path: filepath.Join(dir, name)}).Update(ctx, func([]byte) ([]byte, error) { return data, nil }); err != nil {
			return nil, nil, err
		}
	}
	return pair.Certificate, pair.PrivateKey, nil
}

var _ rt.GatewayBackend = (*GatewayAdapter)(nil)
