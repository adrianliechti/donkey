// Package detect decides how a folder should be built. The same pipeline
// backs `donkey init` and `donkey deploy`:
//
//  1. donkey.yaml with an explicit build.type wins
//  2. a Dockerfile in the folder (the user's Dockerfile always beats a builder)
//  3. a Dockerfile donkey's assistant wrote earlier (.donkey/Dockerfile)
//  4. the installation's builder — railpack's in-process detection, or
//     donkey's own builder, which has the assistant write the Dockerfile
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
		return dockerfileDetection(dockerfile), nil
	case config.BuildRailpack:
		return detectRailpack(dir, app, settings)
	case config.BuildAI:
		return detectAI(dir), nil
	}

	// auto
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err == nil {
		return dockerfileDetection(dockerfile), nil
	}
	// A Dockerfile the assistant wrote earlier is a decision already taken —
	// for this app, by someone who saw it — so it outranks whatever the
	// installation would pick now.
	if _, err := os.Stat(filepath.Join(dir, AIDockerfilePath)); err == nil {
		return &Detection{
			Strategy:       config.BuildDockerfile,
			DockerfilePath: AIDockerfilePath,
			Summary:        "the Dockerfile donkey wrote for this app (" + AIDockerfilePath + ")",
		}, nil
	}
	if settings.BuilderOrDefault() == config.BuildAI {
		return detectAI(dir), nil
	}
	return detectRailpack(dir, app, settings)
}

func dockerfileDetection(path string) *Detection {
	return &Detection{Strategy: config.BuildDockerfile, DockerfilePath: path, Summary: "your " + path}
}

// detectAI reports that donkey will write the Dockerfile itself. Like the
// buildpacks path before it, it does no work here and cannot fail: what the
// app is gets decided by the assistant during the build, and the Dockerfile
// it produces is kept so every later deploy is an ordinary Dockerfile build.
//
// If one is already sitting in the context, that is what gets built — the
// assistant is asked once per app, not once per deploy.
func detectAI(dir string) *Detection {
	if _, err := os.Stat(filepath.Join(dir, AIDockerfilePath)); err == nil {
		return &Detection{
			Strategy:       config.BuildDockerfile,
			DockerfilePath: AIDockerfilePath,
			Summary:        "the Dockerfile donkey wrote for this app (" + AIDockerfilePath + ")",
		}
	}
	return &Detection{
		Strategy: config.BuildAI,
		Summary:  "a Dockerfile donkey writes for this app",
	}
}

// Paths where AI-proposed build files live — donkey never clobbers user files.
const (
	AIDockerfilePath   = ".donkey/Dockerfile"
	AIRailpackConfPath = ".donkey/railpack.json"
)
