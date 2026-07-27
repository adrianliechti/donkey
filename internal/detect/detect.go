// Package detect decides how a folder should be built. The same pipeline
// backs `donkey init` and `donkey deploy`:
//
//  1. donkey.yaml with an explicit build.type wins
//  2. a Dockerfile in the folder (the user's Dockerfile always beats a builder)
//  3. a Dockerfile donkey's assistant wrote earlier (.donkey/Dockerfile)
//  4. donkey's own builder, which has the assistant write the Dockerfile
//
// Every path ends at a Dockerfile. That is the whole design: donkey builds one
// kind of thing, so there is one code path to the build engine, and what an
// app is built with is a file the user can read, edit and keep.
package detect

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/config"
)

type Detection struct {
	Strategy       config.BuildStrategy
	DockerfilePath string // for the dockerfile strategy
	// Summary is a one-line plain-language description of what was found.
	Summary string
}

// Detect works out how to build dir.
func Detect(dir string, app *config.AppConfig) (*Detection, error) {
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
	case config.BuildAI:
		return detectAI(dir), nil
	}

	// auto
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err == nil {
		return dockerfileDetection(dockerfile), nil
	}
	// A Dockerfile the assistant wrote earlier is a decision already taken —
	// for this app, by someone who saw it — so it is what gets built.
	if _, err := os.Stat(filepath.Join(dir, AIDockerfilePath)); err == nil {
		return &Detection{
			Strategy:       config.BuildDockerfile,
			DockerfilePath: AIDockerfilePath,
			Summary:        "the Dockerfile donkey wrote for this app (" + AIDockerfilePath + ")",
		}, nil
	}
	return detectAI(dir), nil
}

func dockerfileDetection(path string) *Detection {
	return &Detection{Strategy: config.BuildDockerfile, DockerfilePath: path, Summary: "your " + path}
}

// detectAI does no work and cannot fail: what the app is gets decided by the
// assistant during the build. One already in the context is what gets built —
// the assistant is asked once per app, not once per deploy.
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

// AIDockerfilePath is where a Dockerfile donkey wrote lives — under .donkey/,
// so a user's own Dockerfile is never clobbered.
const AIDockerfilePath = ".donkey/Dockerfile"
