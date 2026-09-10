package config

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveSpace decides which space an app folder belongs to, most explicit
// first:
//
//  1. what the user said (--space)
//  2. what donkey.yaml says
//  3. the name of the repository the folder is in — so every app of a
//     monorepo lands in one space without anybody configuring it
//  4. the folder's own name, when there is no repository
//
// repoRoot is the repository root folder ("" when the folder isn't in one);
// it is passed in so this stays a pure function.
func ResolveSpace(flag string, app *AppConfig, appDir, repoRoot string) string {
	if flag != "" {
		return flag
	}
	if app != nil && app.Space != "" {
		return app.Space
	}
	if repoRoot != "" {
		return SanitizeSpaceName(filepath.Base(repoRoot))
	}
	abs, err := filepath.Abs(appDir)
	if err != nil {
		return DefaultSpace
	}
	return SanitizeSpaceName(filepath.Base(abs))
}

var nameCleanRE = regexp.MustCompile(`[^a-z0-9-]+`)
var spaceNameCleanRE = regexp.MustCompile(`[^a-z0-9]+`)

// SanitizeName turns a folder name into a name donkey (and Kubernetes) can
// use: lowercase letters, digits and dashes.
func SanitizeName(base string) string {
	name := nameCleanRE.ReplaceAllString(strings.ToLower(base), "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "app"
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	return name
}

// SanitizeSpaceName derives a space from a folder. Explicit space names are
// validated, never silently renamed.
func SanitizeSpaceName(base string) string {
	name := spaceNameCleanRE.ReplaceAllString(strings.ToLower(base), "")
	if name == "" {
		return DefaultSpace
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}
