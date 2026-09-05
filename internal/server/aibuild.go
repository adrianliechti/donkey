package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// Build and rollout failures feed the same repair loop. Its deliverables are
// an ordinary Dockerfile and any source patches used by the verified image.

type buildOutcome struct {
	Result *build.Result
	// Dockerfile is what finally worked, offered back so it can be kept.
	Dockerfile   string
	Dockerignore string
	Changes      []ai.SourceChange
	Attempts     int
}

// buildGenerated owns the whole attempt, including an optional rollout. A
// repository Dockerfile is tried unchanged first; subsequent attempts can
// repair the recipe and isolated source using evidence from either stage.
func (s *Server) buildGenerated(ctx context.Context, out *lineStreamer, builder build.Builder,
	dir string, app *config.AppConfig, req build.Request, release func(*build.Result) error,
) (*buildOutcome, error) {
	attempts := s.cfg.Build.BuildAttempts()
	known := s.knownSecrets(ctx, app.Space, app.Name)
	for _, value := range app.Build.Env {
		known = append(known, value)
	}
	for _, value := range s.cfg.Build.Env {
		known = append(known, value)
	}
	var history []ai.DockerfileAttempt
	outcome := &buildOutcome{}
	var lastErr error
	failedInputs := map[string]int{}
	recipePath := req.DockerfilePath
	originalIgnore := ""
	if recipePath != "" {
		originalIgnore = recipePath + ".dockerignore"
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		out.Update(DeployUpdate{Stage: "building"})
		generated := req.Strategy != config.BuildDockerfile || attempt > 1
		ignore := ""
		if generated {
			summary, err := ai.Summarize(req.ContextDir)
			if err != nil {
				return outcome, fmt.Errorf("reading the build workspace: %w", err)
			}
			if attempt == 1 {
				for _, line := range signalHighlights(summary.Signals) {
					out.Line("· %s", line)
				}
				out.Line("Reading %s and preparing its build…", app.Name)
			} else {
				out.Line("Repairing %s from the failure evidence (attempt %d of %d)…", app.Name, attempt, attempts)
			}
			contract, _ := json.Marshal(struct {
				Host     string                `json:"hostname,omitempty"`
				Command  []string              `json:"commandOverride,omitempty"`
				Env      map[string]string     `json:"runtimeEnvironment,omitempty"`
				Storage  *config.StorageConfig `json:"storage,omitempty"`
				Health   config.Healthcheck    `json:"healthcheck"`
				Services []config.AddonConfig  `json:"services,omitempty"`
			}{config.AppHost(app, app.Space, s.cfg.Domain), app.Command, app.Env, app.Storage, app.Healthcheck, app.Addons})
			result, err := s.assistant.WriteDockerfile(ctx, &ai.DockerfileInput{
				Summary: summary, Root: req.ContextDir, Port: app.Port,
				Instructions: s.cfg.Build.Instructions + buildScope(req.ContextDir, dir),
				Runtime:      string(contract), Attempts: history, KnownSecrets: known,
				AllowEdits: true,
				Progress:   func(line string) { out.Line("· %s", ai.RedactEvidence(line, known...)) },
			})
			if err != nil {
				return outcome, fmt.Errorf("the assistant couldn't prepare this app: %w", err)
			}
			if result == nil || strings.TrimSpace(result.Dockerfile) == "" {
				return outcome, fmt.Errorf("the assistant returned no Dockerfile")
			}
			if result.Notes != "" {
				out.Line("· %s", ai.RedactEvidence(result.Notes, known...))
			}
			if len(result.Changes) > 0 {
				for i := range result.Changes {
					result.Changes[i].Diff = ai.RedactEvidence(result.Changes[i].Diff, known...)
				}
				out.Update(DeployUpdate{Changes: result.Changes})
				outcome.Changes = append(outcome.Changes, result.Changes...)
			}
			if err := writeGeneratedFile(dir, detect.AIDockerfilePath, result.Dockerfile); err != nil {
				return outcome, err
			}
			recipePath, err = filepath.Rel(req.ContextDir, filepath.Join(dir, detect.AIDockerfilePath))
			if err != nil {
				return outcome, err
			}
			recipePath = filepath.ToSlash(recipePath)
			ignore = result.Dockerignore
			if err := writeGeneratedDockerignore(req.ContextDir, recipePath, ignore, originalIgnore); err != nil {
				return outcome, err
			}
			ignore = readDockerfile(req.ContextDir, recipePath+".dockerignore")
			outcome.Dockerfile = result.Dockerfile
		} else {
			outcome.Dockerfile = readDockerfile(req.ContextDir, recipePath)
			ignore = readDockerfile(req.ContextDir, originalIgnore)
			if _, err := os.Stat(filepath.Join(req.ContextDir, originalIgnore)); os.IsNotExist(err) {
				ignore = readDockerfile(req.ContextDir, ".dockerignore")
			}
		}
		outcome.Attempts = attempt
		outcome.Dockerignore = ignore
		out.Update(DeployUpdate{Dockerfile: outcome.Dockerfile, Dockerignore: &ignore, BaseImages: dockerfileBaseImages([]byte(outcome.Dockerfile))})
		attemptReq := req
		attemptReq.Strategy, attemptReq.DockerfilePath = config.BuildDockerfile, recipePath
		// A changed candidate must always create a new runtime generation,
		// even when the registry allows overwriting tags.
		if attempt > 1 {
			attemptReq.ImageRef = fmt.Sprintf("%s-repair-%d", req.ImageRef, attempt)
		}
		result, err := builder.Build(ctx, attemptReq)
		if err == nil && result == nil {
			return outcome, fmt.Errorf("the builder returned no image")
		}
		stage := "build"
		evidence := ""
		if err == nil {
			outcome.Result = result
			out.Line("✓ Image ready: %s", result.ImageRef)
			if release != nil {
				stage = "rollout"
				out.Update(DeployUpdate{Stage: "releasing"})
				err = release(result)
				if err != nil {
					var rollout *runtime.RolloutError
					if !errors.As(err, &rollout) {
						return outcome, err
					}
					evidence = rollout.Evidence.Logs + "\n" + strings.Join(rollout.Evidence.Summaries, "\n") + "\n" + strings.Join(rollout.Evidence.Events, "\n")
				}
			}
		}
		if err == nil {
			return outcome, nil
		}
		lastErr = err
		// Provisioning may have created new service credentials during rollout.
		known = append(known, s.knownSecrets(ctx, app.Space, app.Name)...)
		if stage == "build" {
			evidence = failureText(err, req.Progress.LogPath, known...)
		} else {
			evidence = ai.RedactEvidence(err.Error()+"\n"+tailBytes([]byte(evidence), 12*1024), known...)
		}
		out.Line("· Attempt %d failed during %s. %s", attempt, stage, ai.RedactEvidence(err.Error(), known...))
		history = append(history, ai.DockerfileAttempt{Dockerfile: outcome.Dockerfile, Dockerignore: ignore, Stage: stage, Failure: evidence})
		digest, derr := contextDigest(req.ContextDir)
		if derr != nil {
			return outcome, derr
		}
		// Logs contain changing timestamps and image tags; they do not prove
		// the assistant changed an input to the failing stage.
		fingerprint := digest + "\n" + stage
		failedInputs[fingerprint]++
		if failedInputs[fingerprint] >= 2 {
			return outcome, fmt.Errorf("the same app and build inputs failed twice without progress: %w", lastErr)
		}
	}
	return outcome, fmt.Errorf("the app still could not be verified after %d attempts: %w", attempts, lastErr)
}

// signalHighlights is the short version of what donkey established, for the
// build log. The full picture goes to the assistant; this is the handful of
// lines a person watching a deploy can act on.
func signalHighlights(s *ai.Signals) []string {
	var out []string
	if s == nil {
		return out
	}
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

// Generated exclusions belong to this recipe, never to the shared context.
// Preserve author exclusions verbatim and replace our own exclusions on every
// attempt, including when a correction removes all generated exclusions.
func writeGeneratedDockerignore(contextDir, dockerfile, content, originalIgnore string) error {
	for _, name := range []string{originalIgnore, ".dockerignore"} {
		if name == "" {
			continue
		}
		original, err := os.ReadFile(filepath.Join(contextDir, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			content = string(original)
			break
		}
	}
	return writeGeneratedFile(contextDir, dockerfile+".dockerignore", content)
}

// failureText is what the assistant is shown about a failed attempt: the
// error itself, plus the tail of the build log, which is where the compiler
// message or the startup crash actually is.
func failureText(err error, logPath string, known ...string) string {
	text := err.Error()
	logs, rerr := os.ReadFile(logPath)
	if rerr != nil || len(logs) == 0 {
		return ai.RedactEvidence(text, known...)
	}
	return ai.RedactEvidence(text+"\n\nBuild output:\n"+string(tailBytes(logs, 12*1024)), known...)
}

// verifySpec is what a freshly built image has to prove, or nil when the
// installation turned that off.
func (s *Server) verifySpec(app *config.AppConfig) *build.VerifySpec {
	if !s.cfg.Build.VerifyStartup() || len(app.Addons) > 0 {
		return nil
	}
	env := map[string]string{}
	for key, value := range app.Env {
		if _, _, ref := config.ParseEnvRef(value); ref {
			return nil
		}
		// Such an app must be checked in the runtime where its secrets live.
		entry := key + "=" + value
		if ai.Redact(entry) != entry {
			return nil
		}
		env[key] = value
	}
	var paths []string
	if app.HasStorage() {
		paths = append(paths, app.Storage.Paths...)
	}
	return &build.VerifySpec{
		Env: env, HealthcheckPath: app.Healthcheck.Path, Host: config.AppHost(app, app.Space, s.cfg.Domain), Command: append([]string(nil), app.Command...), StoragePaths: paths,
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
