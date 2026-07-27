// Package detect decides how a folder should be built. The same pipeline
// backs `donkey init` and `donkey deploy`:
//
//  1. donkey.yaml with an explicit build.type wins
//  2. a Dockerfile in the folder (the user's Dockerfile always beats a builder)
//  3. the installation's builder — railpack's in-process detection, or
//     Cloud Native Buildpacks, which detects inside the build instead
//  4. AI-assisted suggestion (M7)
package detect

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
)

type Detection struct {
	Strategy       config.BuildStrategy
	DockerfilePath string // for the dockerfile strategy
	// Railpack carries the in-process plan for the railpack strategy.
	Railpack *build.PlanResult
	// Summary is a one-line plain-language description of what was found.
	Summary string
}

// Detect works out how to build dir. settings is the installation's build
// configuration: it decides which builder an app that brings no Dockerfile
// gets, and it contributes the build environment railpack plans against.
func Detect(dir string, app *config.AppConfig, settings config.BuildSettings) (*Detection, error) {
	buildType := config.BuildAuto
	dockerfile := config.DefaultDockerfile
	if app != nil {
		buildType = app.Build.Type
		if app.Build.Dockerfile != "" {
			dockerfile = app.Build.Dockerfile
		}
	}

	switch buildType {
	case config.BuildDockerfile:
		if _, err := os.Stat(filepath.Join(dir, dockerfile)); err != nil {
			return nil, fmt.Errorf("donkey.yaml says to build with %s, but that file doesn't exist in this folder", dockerfile)
		}
		return &Detection{Strategy: config.BuildDockerfile, DockerfilePath: dockerfile, Summary: "your " + dockerfile}, nil
	case config.BuildRailpack:
		return detectRailpack(dir, app, settings)
	case config.BuildBuildpacks:
		return detectBuildpacks(settings), nil
	}

	// auto
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err == nil {
		return &Detection{Strategy: config.BuildDockerfile, DockerfilePath: dockerfile, Summary: "your " + dockerfile}, nil
	}
	if _, err := os.Stat(filepath.Join(dir, AIDockerfilePath)); err == nil {
		return &Detection{Strategy: config.BuildDockerfile, DockerfilePath: AIDockerfilePath, Summary: "the AI-suggested Dockerfile (.donkey/Dockerfile)"}, nil
	}
	if settings.BuilderOrDefault() == config.BuildBuildpacks {
		return detectBuildpacks(settings), nil
	}
	return detectRailpack(dir, app, settings)
}

// Paths where AI-proposed build files live — donkey never clobbers user files.
const (
	AIDockerfilePath   = ".donkey/Dockerfile"
	AIRailpackConfPath = ".donkey/railpack.json"
)
