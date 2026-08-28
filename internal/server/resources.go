package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	resourceSampleInterval = 15 * time.Second
	resourceSampleTimeout  = 12 * time.Second
)

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	selected, err := telemetry.ParseResourceRange(r.URL.Query().Get("range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.traffic.Resources(r.Context(), space, app, selected)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// resourceTelemetryLoop samples the runtime independently of fleet status
// polling. A failed snapshot never makes the control plane unavailable; the
// console keeps the retained history and marks an old current value stale.
func (s *Server) resourceTelemetryLoop(ctx context.Context) {
	if _, ok := s.rt.(runtime.ResourceSampler); !ok || s.resources == nil {
		return
	}
	ticker := time.NewTicker(resourceSampleInterval)
	defer ticker.Stop()
	lastError := ""
	collect := func() {
		sampleCtx, cancel := context.WithTimeout(ctx, resourceSampleTimeout)
		err := s.collectResourceTelemetry(sampleCtx)
		cancel()
		message := ""
		if err != nil && ctx.Err() == nil {
			message = err.Error()
		}
		if message == lastError {
			return
		}
		if message == "" {
			if lastError != "" {
				log.Print("runtime resource telemetry recovered")
			}
		} else {
			log.Printf("runtime resource telemetry: %s", message)
		}
		lastError = message
	}
	collect()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func (s *Server) collectResourceTelemetry(ctx context.Context) error {
	sampler, ok := s.rt.(runtime.ResourceSampler)
	if !ok {
		return nil
	}
	samples, sampleErr := sampler.ResourceSamples(ctx)
	observations := make([]telemetry.ResourceObservation, 0, len(samples))
	for _, sample := range samples {
		observations = append(observations, telemetry.ResourceObservation{
			Space: sample.Space, App: sample.App, Instance: sample.Instance,
			ContainerID: sample.ContainerID, ContainerName: sample.ContainerName,
			KubeNamespace: sample.KubeNamespace, KubePodName: sample.KubePodName, KubePodUID: sample.KubePodUID,
			SampledAt: sample.SampledAt,
			CPUCores:  sample.CPUCores, CPUUserCores: sample.CPUUserCores,
			CPUSystemCores: sample.CPUSystemCores, CPUModesAvailable: sample.CPUModesAvailable,
			MemoryBytes: sample.MemoryBytes,
		})
	}
	return errors.Join(sampleErr, s.resources.Export(ctx, observations))
}
