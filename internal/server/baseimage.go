package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
)

const maxBaseImageChecks = 8

// Tools run sequentially. Share this bounded cache across repair attempts,
// never across deployments, so each new deployment gets fresh scan evidence.
func (s *Server) baseImageCheck(builder build.Builder, req build.Request, app *config.AppConfig) func(context.Context, string) (string, error) {
	inspector, ok := builder.(build.BaseImageInspector)
	if !ok || req.Scan == nil || req.Scan.Matcher == nil {
		return nil
	}
	cache := map[string]string{}
	checks := 0
	return func(ctx context.Context, image string) (string, error) {
		image = strings.TrimSpace(image)
		if len(image) > 512 {
			return "", fmt.Errorf("image reference is too long")
		}
		named, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return "", fmt.Errorf("invalid image reference: %w", err)
		}
		image = reference.TagNameOnly(named).String()
		if result, ok := cache[image]; ok {
			return result, nil
		}
		if checks >= maxBaseImageChecks {
			return "", fmt.Errorf("base-image check limit reached; use the evidence already collected")
		}
		checks++
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		policy, err := s.scanPolicy(ctx, app.Space, app.Name)
		if err != nil {
			return "", err
		}
		candidate, err := inspector.InspectBaseImage(ctx, image, req.Platform, *req.Scan)
		if err != nil {
			return "", err
		}
		if candidate == nil || candidate.Scan == nil {
			return "", fmt.Errorf("base-image inspection returned no scan")
		}
		eval := evaluateScan(candidate.Scan, policy)
		// Send actionable package facts, not the full SBOM or secret matches.
		type finding struct {
			ID, Severity, Package, Installed, Fixed, Class, Type, Path string
		}
		blocking := make([]finding, 0, min(len(eval.Blocking), 30))
		for _, v := range eval.Blocking[:min(len(eval.Blocking), 30)] {
			blocking = append(blocking, finding{v.ID, v.Severity, v.Package, v.Installed, v.Fixed, v.Class, v.Type, v.Path})
		}
		data, err := json.Marshal(struct {
			*build.BaseImage
			OS               string    `json:"os"`
			EndOfLife        bool      `json:"endOfLife"`
			DBUpdatedAt      string    `json:"databaseUpdatedAt"`
			BlockingCount    int       `json:"blockingCount"`
			BlockingFindings []finding `json:"blockingFindings"`
			OtherFindings    int       `json:"otherFindings"`
			Note             string    `json:"note"`
		}{candidate, candidate.Scan.OS, candidate.Scan.EndOfLife, candidate.Scan.DBUpdatedAt, len(eval.Blocking), blocking,
			len(candidate.Scan.Vulnerabilities) - len(eval.Blocking),
			"Candidate evidence only, before app-specific triage. Use a compatible maintained image and the inspected digest; the complete application image must still pass its own scan."})
		if err != nil {
			return "", err
		}
		cache[image] = string(data)
		return string(data), nil
	}
}
