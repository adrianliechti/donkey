package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// All service consoles use the same protocol and connection checks, regardless
// of the provider that created the binding. Read only the requested service.
func (s *Server) serviceConnection(ctx context.Context, r *http.Request, protocols ...string) (runtime.ServiceConnection, error) {
	space, err := s.space(r)
	if err != nil {
		return runtime.ServiceConnection{}, runtime.UserError{Err: err}
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		return runtime.ServiceConnection{}, runtime.UserError{Err: fmt.Errorf("invalid service name")}
	}
	connection, err := s.rt.ServiceConnection(ctx, space, name)
	if err != nil {
		return runtime.ServiceConnection{}, err
	}
	if len(connection.Binding) == 0 {
		return runtime.ServiceConnection{}, runtime.UserError{Err: fmt.Errorf("the space has no service named %q", name)}
	}
	connection.Protocol = addons.NormalizeProtocol(connection.Protocol)
	if !slices.Contains(protocols, connection.Protocol) {
		return runtime.ServiceConnection{}, runtime.UserError{Err: fmt.Errorf("service %s uses protocol %q, which this console does not support", name, connection.Protocol)}
	}
	if err := addons.ValidateBinding(connection.Protocol, connection.Binding); err != nil {
		return runtime.ServiceConnection{}, runtime.UserError{Err: err}
	}
	return connection, nil
}
