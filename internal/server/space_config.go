package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type identityContextKey struct{}
type spaceConfigsContextKey struct{}

var errSpaceForbidden = errors.New("you do not have permission to manage this space")

func requestIdentity(ctx context.Context) *identity.Identity {
	id, _ := ctx.Value(identityContextKey{}).(*identity.Identity)
	return id
}

func (s *Server) platformAdmin(ctx context.Context) bool {
	settings, _ := ctx.Value(platformContextKey{}).(config.PlatformConfig)
	return settings.IsAdmin(requestIdentity(ctx))
}

func (s *Server) spaceRole(ctx context.Context, space string) (string, error) {
	if s.platformAdmin(ctx) {
		return config.SpaceAdmin, nil
	}
	id := requestIdentity(ctx)
	if id == nil {
		return "", errSpaceForbidden
	}
	if settings, ok := ctx.Value(spaceConfigsContextKey{}).(map[string]config.SpaceConfig); ok {
		return settings[space].Role(id), nil
	}
	access, err := s.rt.SpaceConfig(ctx, space)
	if errors.Is(err, runtime.ErrSpaceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return access.Role(id), nil
}

// The context value lasts for one fleet request. Permission reads are batched
// where the runtime supports it, but never reused across HTTP requests.
func (s *Server) listSpaceConfigs(ctx context.Context) (map[string]config.SpaceConfig, error) {
	if provider, ok := s.rt.(runtime.SpaceConfigsProvider); ok {
		return provider.ListSpaceConfigs(ctx)
	}
	spaces, err := s.rt.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	settings := make(map[string]config.SpaceConfig, len(spaces))
	for _, space := range spaces {
		policy, err := s.rt.SpaceConfig(ctx, space)
		if err != nil {
			return nil, err
		}
		settings[space] = policy
	}
	return settings, nil
}

func (s *Server) authorizeSpace(ctx context.Context, space string, admin bool) error {
	if err := s.rt.ValidateSpace(space); err != nil {
		return err
	}
	role, err := s.spaceRole(ctx, space)
	if err != nil {
		return err
	}
	if (role != config.SpaceAdmin && role != config.SpaceDeveloper) || (admin && role != config.SpaceAdmin) {
		return errSpaceForbidden
	}
	return nil
}

func writeAccessError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, errSpaceForbidden) {
		status = http.StatusForbidden
	} else if errors.Is(err, runtime.ErrSpaceNotFound) {
		status = http.StatusNotFound
	}
	writeErr(w, status, err)
}

func (s *Server) requireSpace(w http.ResponseWriter, r *http.Request, space string, admin bool) bool {
	if err := s.authorizeSpace(r.Context(), space, admin); err != nil {
		writeAccessError(w, err)
		return false
	}
	return true
}

// Every path-addressed space operation is checked here, including GETs that
// expose credentials or open tunnels. Uploads and plans check their resolved
// space in their handlers, after reading the actual deployment target.
func (s *Server) authorizeRoute(w http.ResponseWriter, r *http.Request) bool {
	if space := r.PathValue("space"); space != "" {
		if _, err := s.space(r); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return false
		}
		if viewerRoute(r.Pattern) {
			role, err := s.spaceRole(r.Context(), space)
			if err != nil {
				writeAccessError(w, err)
				return false
			}
			if role == "" {
				writeAccessError(w, errSpaceForbidden)
				return false
			}
			return true
		}
		admin := r.Pattern == "DELETE /v1/spaces/{space}" ||
			r.Pattern == "GET /v1/spaces/{space}/settings" ||
			r.Pattern == "PUT /v1/spaces/{space}/settings"
		return s.requireSpace(w, r, space, admin)
	}
	switch r.Pattern {
	case "GET /v1/ping", "GET /v1/apps", "GET /v1/spaces", "GET /v1/fleet", "GET /v1/catalog",
		"POST /v1/plans", "PUT /v1/plans/{id}", "POST /v1/deploys", "GET /v1/deploys",
		"POST /v1/projects/plans", "PUT /v1/projects/plans/{id}", "POST /v1/projects/deploys":
		return true
	default:
		if !s.platformAdmin(r.Context()) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("this action requires a platform administrator"))
			return false
		}
		return true
	}
}

func (s *Server) handleGetSpaceConfig(w http.ResponseWriter, r *http.Request) {
	access, err := s.rt.SpaceConfig(r.Context(), r.PathValue("space"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, access.NormalizeOfferings())
}

func (s *Server) handlePutSpaceConfig(w http.ResponseWriter, r *http.Request) {
	var access config.SpaceConfig
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 160<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&access); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if access.Domains == nil {
		access.Domains = []string{}
	}
	access = access.NormalizeOfferings()
	if err := access.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	previous, err := s.rt.SpaceConfig(r.Context(), r.PathValue("space"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if err := s.validateSpaceOfferings(r.Context(), &access, previous); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.platformAdmin(r.Context()) {
		id := requestIdentity(r.Context())
		if id == nil || access.Role(id) != config.SpaceAdmin {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("keep yourself as a space admin, or ask a platform administrator to remove your access"))
			return
		}
	}
	if err := s.checkExistingDomains(r.Context(), r.PathValue("space"), access); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.rt.SetSpaceConfig(r.Context(), r.PathValue("space"), access); err != nil {
		writeAccessError(w, err)
		return
	}
	if !access.AllowPublic {
		if err := runtime.ReconcileAccess(r.Context(), s.rt, r.PathValue("space")); err != nil {
			writeAccessError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, access)
}

// Narrowing the allowlist must not leave already-published hosts outside it.
// Move those apps first; a policy edit never silently takes an app offline.
func (s *Server) checkExistingDomains(ctx context.Context, space string, policy config.SpaceConfig) error {
	if len(policy.Domains) == 0 {
		return nil
	}
	apps, err := s.rt.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.Space != space {
			continue
		}
		network, err := s.rt.Network(ctx, space, app.App)
		if err != nil {
			return err
		}
		hosts := []string{network.Host}
		for _, route := range network.Routes {
			hosts = append(hosts, route.Host)
		}
		for _, host := range hosts {
			if err := policy.CheckHost(host); err != nil {
				return fmt.Errorf("app %s uses %s — change its hostname before narrowing the allowlist", app.App, host)
			}
		}
	}
	return nil
}
