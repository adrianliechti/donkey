package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

// analyzeApp reads the uploaded folder with the assistant — donkey's own
// detector is good at "which language is this", and poor at "which folder
// holds data that must survive a restart". The two answer different questions,
// so the detector's verdict is handed to the assistant as evidence.
//
// A failure here fails the deploy: donkey would otherwise apply a half-read
// app (no disk for the folder holding the database, say) without anyone
// noticing.
func (s *Server) analyzeApp(ctx context.Context, dir string, det *detect.Detection, detErr error) (*ai.Analysis, error) {
	sum, err := ai.Summarize(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the uploaded folder: %w", err)
	}
	in := &ai.AnalyzeInput{Summary: sum, DetectionFailed: detErr != nil}
	switch {
	case detErr != nil:
		in.Detected = detErr.Error()
	case det != nil:
		in.Detected = det.Summary
	}
	if b, err := os.ReadFile(filepath.Join(dir, config.AppFileName)); err == nil {
		in.CurrentConfig = string(b)
	}
	analysis, err := s.assistant.Analyze(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("the assistant couldn't read this app: %w", err)
	}
	return analysis, nil
}

// rescueDetection writes the assistant's proposed build file into the (throw
// away) build context so railpack or BuildKit can use it. The user's own
// files are never touched — this folder only exists for this one build.
func rescueDetection(dir string, analysis *ai.Analysis) error {
	proposal := buildProposal(analysis)
	if proposal == nil {
		return fmt.Errorf("no proposal available")
	}
	return writeProposal(dir, proposal)
}

// proposedPort works out which port the app will be given, and where that
// answer came from — the assistant's reading of the code first, then what the
// detected stack conventionally listens on. An app that honors PORT takes
// whatever donkey sets, so any answer is right and this one is tidy.
func proposedPort(det *detect.Detection, analysis *ai.Analysis) (int, string) {
	if analysis != nil && analysis.Port > 0 {
		return analysis.Port, PortFromCode
	}
	source := PortGuessed
	if analysis != nil && analysis.PortReason != "" {
		// The assistant looked and found no fixed port: the app follows the
		// PORT convention, so donkey's choice is the app's port.
		source = PortFromEnv
	}
	if det != nil && det.Railpack != nil {
		for _, p := range det.Railpack.Providers {
			switch p {
			case "python":
				return 8000, source
			case "node", "ruby":
				return 3000, source
			case "golang", "go", "java", "php":
				return 8080, source
			case "staticfile":
				return 80, source
			}
		}
	}
	return 8080, source
}

// Where a port came from, for a review screen that would rather explain than
// ask.
const (
	PortFromConfig = "config"
	PortFromCode   = "code"
	PortFromEnv    = "env"
	PortGuessed    = "guess"
)
