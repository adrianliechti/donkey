package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

// donkey's own builder: the assistant writes the app's Dockerfile, donkey
// builds it, and the image has to start before it is published.
//
// The loop is the point. A model writing a Dockerfile from a file tree gets
// it wrong in ways that only show up when something runs — an interpreter
// that is not on PATH, a dependency the build never installed, a start
// command that exits at once. Every one of those produces an error, and an
// error is something the model can be shown. So a failure is not the end of
// the deploy, it is the next prompt.
//
// What comes out is an ordinary Dockerfile, which is the other half of the
// idea: it is written into the app's folder, so the app is read once and
// every later deploy is a plain, repeatable Dockerfile build with no model in
// the path at all.

// aiBuildOutcome is a finished (or abandoned) run of that loop.
type aiBuildOutcome struct {
	Result *build.Result
	// Dockerfile is what finally worked — offered back so it can be kept.
	Dockerfile string
	// Attempts is how many the assistant needed; 1 means it got it first go.
	Attempts int
	// NeverListened is set when the image started and stayed up but never
	// opened the port it is about to be deployed on — allowed through, and
	// worth saying out loud.
	NeverListened bool
}

// buildWithAI writes a Dockerfile, builds it, and keeps going until the image
// builds and starts or the attempts run out.
func (s *Server) buildWithAI(ctx context.Context, out *lineStreamer, builder *build.BuildKitBuilder,
	dir string, app *config.AppConfig, req build.Request,
) (*aiBuildOutcome, error) {
	summary, err := ai.Summarize(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the uploaded folder: %w", err)
	}

	attempts := s.cfg.Build.AIAttempts()
	var previous, failure string
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt == 1 {
			out.Line("Writing a Dockerfile for %s…", app.Name)
		} else {
			out.Line("That didn't work. Rewriting the Dockerfile (attempt %d of %d)…", attempt, attempts)
		}
		dockerfile, err := s.assistant.WriteDockerfile(ctx, &ai.DockerfileInput{
			Summary:  summary,
			Port:     int(app.Port),
			Previous: previous,
			Failure:  failure,
		})
		if err != nil {
			// The assistant not answering is a different problem from a
			// Dockerfile that doesn't work, and retrying won't fix it.
			return nil, fmt.Errorf("the assistant couldn't write a Dockerfile for this app: %w", err)
		}
		// Into the throwaway build context, never the user's own folder —
		// that copy only exists for this build, and what finally works
		// travels back in the result instead.
		if err := writeAIFile(dir, detect.AIDockerfilePath, dockerfile); err != nil {
			return nil, err
		}

		attemptReq := req
		attemptReq.Strategy = config.BuildDockerfile
		attemptReq.DockerfilePath = detect.AIDockerfilePath
		result, buildErr := builder.Build(ctx, attemptReq)
		if buildErr == nil {
			outcome := &aiBuildOutcome{Result: result, Dockerfile: dockerfile, Attempts: attempt}
			if logs, rerr := os.ReadFile(req.Progress.LogPath); rerr == nil &&
				strings.Contains(string(logs), build.VerifyNeverListened) {
				outcome.NeverListened = true
			}
			return outcome, nil
		}

		lastErr = buildErr
		previous = dockerfile
		// The build log is what the model needs: the error the deploy would
		// otherwise show a person, and nothing else.
		failure = aiFailureText(buildErr, req.Progress.LogPath)
	}
	return nil, fmt.Errorf("donkey wrote %d Dockerfiles for this app and none of them produced a working image — the last failure was: %w",
		attempts, lastErr)
}

// aiFailureText is what the assistant is shown about a failed attempt: the
// error itself, plus the tail of the build log, which is where the compiler
// message or the startup crash actually is.
func aiFailureText(err error, logPath string) string {
	text := err.Error()
	logs, rerr := os.ReadFile(logPath)
	if rerr != nil || len(logs) == 0 {
		return text
	}
	return text + "\n\nBuild output:\n" + string(tailBytes(logs, 12*1024))
}

// aiVerifySpec is what a freshly built image has to prove, or nil when the
// installation turned that off.
func (s *Server) aiVerifySpec(app *config.AppConfig) *build.VerifySpec {
	if !s.cfg.Build.VerifyStartup() {
		return nil
	}
	return &build.VerifySpec{
		Port:    int(app.Port),
		Seconds: s.cfg.Build.VerifySeconds(),
	}
}

// writeAIFile stores a generated build file under .donkey/ in the build
// context.
func writeAIFile(dir, relPath, content string) error {
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
