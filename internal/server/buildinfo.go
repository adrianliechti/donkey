package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/detect"
)

// describePlanBuild keeps the deterministic part of repository analysis in
// the plan. The assistant still explains the app itself; these facts come
// straight from manifests, lockfiles and the Dockerfile and can therefore be
// shown as evidence during the release.
func describePlanBuild(dir string, det *detect.Detection, signals *ai.Signals) PlanBuild {
	out := PlanBuild{}
	if det != nil {
		out.Summary = det.Summary
		out.Strategy = string(det.Strategy)
		if det.DockerfilePath != "" {
			out.BaseImages = dockerfileBaseImagesAt(dir, det.DockerfilePath)
		}
	}
	if signals == nil {
		return out
	}
	out.Runtimes = planSignals(signals.Runtimes)
	out.Frameworks = planSignals(signals.Frameworks)
	if signals.Static != nil {
		value := signals.Static.Framework
		if signals.Static.OutputDir != "" {
			output := signals.Static.OutputDir
			if output != "." {
				output = "./" + strings.TrimPrefix(output, "./")
			}
			value += " → " + output
		}
		found := false
		for i := range out.Frameworks {
			if strings.EqualFold(out.Frameworks[i].Value, signals.Static.Framework) {
				out.Frameworks[i] = PlanSignal{Value: value, Evidence: signals.Static.Evidence}
				found = true
				break
			}
		}
		if !found {
			out.Frameworks = append(out.Frameworks, PlanSignal{Value: value, Evidence: signals.Static.Evidence})
		}
	}
	out.Versions = planSignals(signals.Versions)
	out.PackageManagers = planSignals(signals.PackageManager)
	for _, p := range signals.SystemPackages {
		out.SystemPackages = append(out.SystemPackages, PlanSystemPackage{
			Dependency: strings.TrimSuffix(p.Dependency, "["),
			Build:      append([]string(nil), p.Build...),
			Runtime:    append([]string(nil), p.Runtime...),
		})
	}
	return out
}

func planSignals(in []ai.Signal) []PlanSignal {
	out := make([]PlanSignal, 0, len(in))
	for _, signal := range in {
		out = append(out, PlanSignal{Value: signal.Value, Evidence: signal.Evidence})
	}
	return out
}

func dockerfileBaseImagesAt(dir, name string) []string {
	full := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	source, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	return dockerfileBaseImages(source)
}

var dockerfileVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// dockerfileBaseImages reads the FROM instructions without trying to become
// a Dockerfile evaluator. It resolves declared ARG defaults, ignores stage
// aliases used by later stages, and leaves unresolved build arguments visible
// instead of pretending to know their runtime value.
func dockerfileBaseImages(source []byte) []string {
	args := map[string]string{}
	stages := map[string]bool{}
	seen := map[string]bool{}
	var out []string

	for _, line := range dockerfileLogicalLines(string(source)) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			key, value, ok := strings.Cut(fields[1], "=")
			if ok && key != "" {
				args[key] = strings.Trim(value, `"'`)
			}
		case "FROM":
			i := 1
			for i < len(fields) && strings.HasPrefix(fields[i], "--") {
				i++
			}
			if i >= len(fields) {
				continue
			}
			image := expandDockerfileArgs(fields[i], args)
			if image != "" && !stages[strings.ToLower(image)] && !seen[image] {
				seen[image] = true
				out = append(out, image)
			}
			if i+2 < len(fields) && strings.EqualFold(fields[i+1], "as") {
				stages[strings.ToLower(fields[i+2])] = true
			}
		}
	}
	return out
}

func expandDockerfileArgs(value string, args map[string]string) string {
	return dockerfileVariable.ReplaceAllStringFunc(value, func(match string) string {
		parts := dockerfileVariable.FindStringSubmatch(match)
		name := parts[1]
		if name == "" {
			name = parts[2]
		}
		if replacement, ok := args[name]; ok {
			return replacement
		}
		return match
	})
}

func dockerfileLogicalLines(source string) []string {
	physical := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	var out []string
	logical := ""
	for _, line := range physical {
		trimmed := strings.TrimSpace(line)
		if logical == "" && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		continued := strings.HasSuffix(trimmed, "\\")
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "\\"))
		if logical != "" {
			logical += " "
		}
		logical += trimmed
		if continued {
			continue
		}
		if logical != "" {
			out = append(out, logical)
		}
		logical = ""
	}
	if logical != "" {
		out = append(out, logical)
	}
	return out
}
