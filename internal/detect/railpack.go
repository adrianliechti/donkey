package detect

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
)

// detectRailpack runs railpack's in-process detection and planning, honoring
// an AI-proposed railpack config when one was accepted earlier.
func detectRailpack(dir string, app *config.AppConfig) (*Detection, error) {
	var buildEnv map[string]string
	if app != nil {
		buildEnv = app.Build.Env
	}
	configPath := ""
	if _, err := os.Stat(filepath.Join(dir, AIRailpackConfPath)); err == nil {
		configPath = AIRailpackConfPath
	}
	plan, err := build.GeneratePlan(dir, buildEnv, configPath)
	if err != nil {
		return nil, fmt.Errorf(
			"%w\n\nDonkey looked for a Dockerfile and for a known app type (Python, Node, Go, …). You can add a Dockerfile, or let donkey's assistant propose one", err)
	}
	return &Detection{
		Strategy: config.BuildRailpack,
		Railpack: plan,
		Summary:  "automatic detection (" + plan.ProvidersSummary() + ")",
	}, nil
}
