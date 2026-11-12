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

type platformContextKey struct{}

func (s *Server) loadPlatformConfig(ctx context.Context, id *identity.Identity) (config.PlatformConfig, error) {
	settings, err := s.rt.PlatformConfig(ctx)
	if !errors.Is(err, runtime.ErrPlatformNotFound) {
		return settings, err
	}
	initial := config.FirstPlatformAdmin(id)
	if !s.cfg.PlatformAdmins.Empty() {
		initial.Admins = *s.cfg.PlatformAdmins
	}
	return s.rt.InitPlatformConfig(ctx, initial)
}

func (s *Server) handleGetPlatformConfig(w http.ResponseWriter, r *http.Request) {
	settings, _ := r.Context().Value(platformContextKey{}).(config.PlatformConfig)
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handlePutPlatformConfig(w http.ResponseWriter, r *http.Request) {
	var settings config.PlatformConfig
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 160<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := settings.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !settings.IsAdmin(requestIdentity(r.Context())) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("keep yourself as a platform admin; another admin can remove your access"))
		return
	}
	if err := s.rt.SetPlatformConfig(r.Context(), settings); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
