package build

import (
	"fmt"
	"strings"

	rpcore "github.com/railwayapp/railpack/core"
	rpapp "github.com/railwayapp/railpack/core/app"
)

// PlanResult is donkey's view of railpack's in-process detection: the build
// plan plus what was recognized, for UX and AI context.
type PlanResult struct {
	Result    *rpcore.BuildResult
	Providers []string
}

// GeneratePlan runs railpack's detection and planning on dir — entirely
// in-process, no railpack binary. configFilePath optionally points at a
// railpack config (e.g. one proposed by the AI assistant).
func GeneratePlan(dir string, buildEnv map[string]string, configFilePath string) (*PlanResult, error) {
	a, err := rpapp.NewApp(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the app folder: %w", err)
	}
	if buildEnv == nil {
		buildEnv = map[string]string{}
	}
	env := rpapp.NewEnvironment(&buildEnv)
	res := rpcore.GenerateBuildPlan(a, env, &rpcore.GenerateBuildPlanOptions{
		ConfigFilePath: configFilePath,
	})
	if res == nil || !res.Success || res.Plan == nil {
		return nil, planFailure(res)
	}
	return &PlanResult{Result: res, Providers: res.DetectedProviders}, nil
}

func planFailure(res *rpcore.BuildResult) error {
	msg := "donkey couldn't recognize the app in this folder"
	if res != nil {
		var details []string
		for _, l := range res.Logs {
			details = append(details, l.Msg)
		}
		if len(details) > 0 {
			msg += ": " + strings.Join(details, "; ")
		}
	}
	return fmt.Errorf("%s", msg)
}

// ProvidersSummary is a plain-language "Python, Node" style list.
func (p *PlanResult) ProvidersSummary() string {
	if len(p.Providers) == 0 {
		return "automatic detection"
	}
	return strings.Join(p.Providers, ", ")
}
