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

// donkey's builder: the assistant writes the app's Dockerfile, donkey builds
// it, and the image must start before it is published. A failure is not the
// end of the deploy — it is the next prompt. What comes out is an ordinary
// Dockerfile, so the app is read once and every later deploy is a plain,
// repeatable build with no model in the path.

type buildOutcome struct {
	Result *build.Result
	// Dockerfile is what finally worked, offered back so it can be kept.
	Dockerfile string
	Attempts   int
	// NeverListened: up and stayed up, but never opened the port it is about
	// to be deployed on. Allowed through, and worth saying out loud.
	NeverListened bool
}

// buildGenerated keeps going until the image builds and starts, or the
// attempts run out.
func (s *Server) buildGenerated(ctx context.Context, out *lineStreamer, builder *build.BuildKitBuilder,
	dir string, app *config.AppConfig, req build.Request,
) (*buildOutcome, error) {
	summary, err := ai.Summarize(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the uploaded folder: %w", err)
	}
	if sig := summary.Signals; sig != nil {
		// What donkey worked out on its own is worth showing: it is the part
		// of the decision that is not a model's judgement, and when the build
		// goes wrong it is the first thing to check.
		for _, line := range signalHighlights(sig) {
			out.Line("· %s", line)
		}
	}

	attempts := s.cfg.Build.BuildAttempts()
	var history []ai.DockerfileAttempt
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt == 1 {
			out.Line("Reading %s and writing it a Dockerfile…", app.Name)
		} else {
			out.Line("That didn't work. Reading the failure and rewriting the Dockerfile (attempt %d of %d)…", attempt, attempts)
		}
		result, err := s.assistant.WriteDockerfile(ctx, &ai.DockerfileInput{
			Summary: summary,
			// The uploaded copy, never the user's working tree: the assistant
			// reads it directly, so what it writes is based on the app rather
			// than on a summary of it.
			Root:         dir,
			Port:         int(app.Port),
			Instructions: s.cfg.Build.Instructions,
			Attempts:     history,
		})
		if err != nil {
			// The assistant not answering is a different problem from a
			// Dockerfile that doesn't work, and retrying won't fix it.
			return nil, fmt.Errorf("the assistant couldn't write a Dockerfile for this app: %w", err)
		}
		if len(result.FilesRead) > 0 {
			out.Line("· Read %s", strings.Join(result.FilesRead, ", "))
		}
		if result.Notes != "" {
			out.Line("· %s", result.Notes)
		}
		out.Update(DeployUpdate{BaseImages: dockerfileBaseImages([]byte(result.Dockerfile))})
		// Into the throwaway build context, never the user's own folder —
		// that copy only exists for this build, and what finally works
		// travels back in the result instead.
		if err := writeGeneratedFile(dir, detect.AIDockerfilePath, result.Dockerfile); err != nil {
			return nil, err
		}
		if err := writeGeneratedDockerignore(dir, result.Dockerignore); err != nil {
			return nil, err
		}

		attemptReq := req
		attemptReq.Strategy = config.BuildDockerfile
		attemptReq.DockerfilePath = detect.AIDockerfilePath
		buildResult, buildErr := builder.Build(ctx, attemptReq)
		if buildErr == nil {
			outcome := &buildOutcome{Result: buildResult, Dockerfile: result.Dockerfile, Attempts: attempt}
			if logs, rerr := os.ReadFile(req.Progress.LogPath); rerr == nil &&
				strings.Contains(string(logs), build.VerifyNeverListened) {
				outcome.NeverListened = true
			}
			return outcome, nil
		}

		lastErr = buildErr
		history = append(history, ai.DockerfileAttempt{
			Dockerfile: result.Dockerfile,
			// The build log is what the model needs: the error the deploy would
			// otherwise show a person, and nothing else.
			Failure: failureText(buildErr, req.Progress.LogPath),
		})
	}
	return nil, fmt.Errorf("donkey wrote %d Dockerfiles for this app and none of them produced a working image — the last failure was: %w",
		attempts, lastErr)
}

// signalHighlights is the short version of what donkey established, for the
// build log. The full picture goes to the assistant; this is the handful of
// lines a person watching a deploy can act on.
func signalHighlights(s *ai.Signals) []string {
	var out []string
	if len(s.Runtimes) > 0 {
		out = append(out, "Looks like "+s.Runtimes[0].Value+" ("+s.Runtimes[0].Evidence+")")
	}
	if len(s.PackageManager) > 0 {
		out = append(out, "Dependencies: "+s.PackageManager[0].Value+" ("+s.PackageManager[0].Evidence+")")
	}
	if st := s.Static; st != nil && st.CustomStart == "" {
		line := "Static front end built by " + st.Framework
		if st.OutputDir != "" {
			line += " into ./" + st.OutputDir
		}
		out = append(out, line)
	}
	for _, p := range s.SystemPackages {
		out = append(out, "Note: "+p.Dependency+" needs OS packages installed in the image")
	}
	return out
}

// writeGeneratedDockerignore installs the assistant's proposed exclusions —
// but only when the app brought none of its own. A .dockerignore the author
// wrote is a decision already taken, and overwriting it with a guess is how a
// file the build needs stops arriving.
func writeGeneratedDockerignore(dir, content string) error {
	if content == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".dockerignore")); err == nil {
		return nil
	}
	// The Dockerfile-specific name, which is what BuildKit's frontend looks
	// for first — so this applies to donkey's build and to nothing else.
	return writeGeneratedFile(dir, ".dockerignore", content)
}

// failureText is what the assistant is shown about a failed attempt: the
// error itself, plus the tail of the build log, which is where the compiler
// message or the startup crash actually is.
func failureText(err error, logPath string) string {
	text := err.Error()
	logs, rerr := os.ReadFile(logPath)
	if rerr != nil || len(logs) == 0 {
		return text
	}
	return text + "\n\nBuild output:\n" + string(tailBytes(logs, 12*1024))
}

// verifySpec is what a freshly built image has to prove, or nil when the
// installation turned that off.
func (s *Server) verifySpec(app *config.AppConfig) *build.VerifySpec {
	if !s.cfg.Build.VerifyStartup() {
		return nil
	}
	return &build.VerifySpec{
		Port:    int(app.Port),
		Seconds: s.cfg.Build.VerifyWindow(),
	}
}

// writeGeneratedFile stores a generated build file under .donkey/ in the build
// context.
func writeGeneratedFile(dir, relPath, content string) error {
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
