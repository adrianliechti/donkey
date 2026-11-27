package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

// Each candidate is tried independently, alongside the proposed selection and
// the current inventory of the target space. No service is created by a preview.
func (s *Server) handleServiceOptions(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Selected   []runtime.ServiceChoice `json:"selected"`
		Candidates []runtime.ServiceChoice `json:"candidates"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Candidates) > 256 || len(body.Selected) > 256 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("a service preview supports up to 256 selected services and 256 candidates"))
		return
	}
	ctx := r.Context()
	selection, err := runtime.ReadServiceSelection(ctx, s.rt, s.cfg.ServicePolicy, space)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	blocked := map[string]string{}
	for _, candidate := range body.Candidates {
		if err := selection.Check(ctx, append(body.Selected, candidate)); err != nil {
			blocked[candidate.Name] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": blocked})
}
