package build

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	rpmise "github.com/railwayapp/railpack/core/mise"
)

// railpack resolves every package version by shelling out to mise, which it
// downloads from GitHub the first time — inside donkey-server, at plan time.
// That download is an ordinary http.Get on the default transport, so it
// honours HTTPS_PROXY and needs nothing from donkey. donkey's image does not
// carry a copy: mise is 90 MB, and it is railpack's dependency alone.
//
// What railpack leaves no room for is the *exec*. It builds mise's child
// environment from scratch (core/mise/mise.go), so a proxy set on the server
// never reaches `mise latest`, and version resolution fails behind one even
// though the download succeeded. The only opening is the path railpack checks
// before downloading: put a shim there and it runs instead, re-exporting what
// railpack dropped before handing over to the real binary.
//
// So this is opt-in. An installation that gives donkey a mise binary
// (DONKEY_MISE_BINARY, or a copy at bakedMisePath in a derived image) gets it
// staged — as a shim when there is a proxy or a mirror to inject, as a plain
// copy otherwise, which also spares railpack the download. An installation
// that gives none is left alone: railpack fetches its own, exactly as before.
//
// Note the limit none of this lifts. railpack asks mise for the latest
// matching version even when a version is pinned exactly, and most mise
// backends answer that from the GitHub API, which a registry mirror cannot
// serve. With a forward proxy this works; on a mirror-only network railpack's
// planning still fails, and the answer there is build.builder: buildpacks.
const (
	// bakedMiseEnv points donkey at a mise binary to stage.
	bakedMiseEnv = "DONKEY_MISE_BINARY"
	// bakedMisePath is where a derived image would put one.
	bakedMisePath = "/usr/local/lib/donkey/mise"
)

// MiseVersion is the mise release the linked railpack insists on. It moves
// when railpack is upgraded, which is exactly why nothing here hard-codes it.
func MiseVersion() string { return rpmise.Version }

// MisePath is the exact path railpack checks before it downloads anything —
// derived the same way railpack derives it, so a railpack upgrade cannot
// leave donkey putting the binary somewhere nothing looks.
func MisePath() string { return misePath(rpmise.InstallDir) }

func misePath(installDir string) string {
	name := "mise-" + rpmise.Version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(installDir, name)
}

// BakedMisePath is donkey's own copy of the mise binary.
func BakedMisePath() string {
	if p := os.Getenv(bakedMiseEnv); p != "" {
		return p
	}
	return bakedMisePath
}

// EnsureMise stages a mise where railpack looks, when the installation
// provided one. buildEnv is the installation's build environment: its MISE_*
// and GITHUB_TOKEN entries are what the resolver needs to reach a mirror, and
// they only reach it through the shim.
//
// Providing none is the ordinary case and not an error — railpack downloads
// its own. It is safe to call more than once.
func EnsureMise(ctx context.Context, buildEnv map[string]string) error {
	return ensureMise(ctx, rpmise.InstallDir, BakedMisePath(), buildEnv)
}

func ensureMise(ctx context.Context, installDir, baked string, buildEnv map[string]string) error {
	target := misePath(installDir)
	// What should be there depends on the configuration, so "already there"
	// is only good enough if it is the right thing. A container gets a fresh
	// /tmp every start and this never matters; a server running straight on a
	// host would otherwise keep a shim written before the proxy was set, and
	// railpack would go on failing with nothing to show why.
	want := ""
	if inject := miseInjectedEnv(buildEnv); len(inject) > 0 {
		want = miseShim(baked, inject)
	}
	if current, err := os.ReadFile(target); err == nil {
		if want == "" || string(current) == want {
			return nil
		}
	}
	if _, err := os.Stat(baked); err != nil {
		// Nothing to stage: railpack will fetch its own.
		return nil
	}
	if err := checkMiseVersion(ctx, baked); err != nil {
		return err
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("couldn't prepare %s: %w", installDir, err)
	}

	if want == "" {
		return copyExecutable(baked, target)
	}
	return os.WriteFile(target, []byte(want), 0o755)
}

// CheckMise describes what railpack's version resolver will do, for doctor.
//
// A staged mise is reported with its path. None staged is the ordinary case
// and not a failure — it is reported as what it is, so an admin on a
// restricted network can see the egress before a deploy discovers it. Only a
// binary that was provided and cannot be used is an error, because that is a
// setting someone got wrong.
func CheckMise(ctx context.Context) (string, error) {
	return checkMise(ctx, rpmise.InstallDir, BakedMisePath())
}

func checkMise(ctx context.Context, installDir, baked string) (string, error) {
	target := misePath(installDir)
	// Whatever sits at railpack's path is what railpack will run — donkey's
	// copy, donkey's shim, or one railpack fetched itself. Asking that,
	// rather than the binary donkey was handed, is the question worth
	// answering: a shim reports the version of the binary it execs.
	if _, err := os.Stat(target); err == nil {
		if err := checkMiseVersion(ctx, target); err != nil {
			return "", err
		}
		return "ready at " + target, nil
	}
	if _, err := os.Stat(baked); err == nil {
		return "", fmt.Errorf("mise at %s was not staged to %s", baked, target)
	}
	return fmt.Sprintf(
		"railpack downloads mise %s from github.com at first deploy — set %s to a local copy, or use build.builder: buildpacks",
		rpmise.Version, bakedMiseEnv), nil
}

// checkMiseVersion fails loudly on a mismatch. railpack picks the binary by
// filename, so a wrong version would be run under the right name and the
// mismatch would only surface as a strange failure mid-deploy.
func checkMiseVersion(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return fmt.Errorf("the bundled mise at %s didn't run: %w", path, err)
	}
	// "2026.7.11 linux-x64 (abc1234 2026-07-11)"
	got, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	got = strings.TrimPrefix(got, "v")
	if got != rpmise.Version {
		return fmt.Errorf(
			"the bundled mise at %s is version %s, but the linked railpack runs mise %s — rebuild donkey's image against this railpack",
			path, got, rpmise.Version)
	}
	return nil
}

// miseInjectedEnv is what the shim has to put back. The proxy comes from
// donkey-server's own environment, where the installation put it; the mirror
// settings come from the build environment, which is where an admin writes
// everything else about reaching a package registry.
func miseInjectedEnv(buildEnv map[string]string) map[string]string {
	inject := map[string]string{}
	for _, k := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	} {
		if v := os.Getenv(k); v != "" {
			inject[k] = v
		}
	}
	for k, v := range buildEnv {
		// MISE_* is how mise itself is pointed at a mirror
		// (MISE_NODE_MIRROR_URL and friends); the token is what keeps its
		// GitHub-backed lookups from being rate-limited into failure.
		if strings.HasPrefix(k, "MISE_") || k == "GITHUB_TOKEN" {
			inject[k] = v
		}
	}
	return inject
}

// miseShim re-exports what railpack dropped and hands over to the real
// binary. railpack builds mise's environment from scratch and passes
// MISE_NO_CONFIG=1, so this script is the only place these settings can be
// put back — it is deliberately the smallest thing that can be: no logic, no
// state, and it keeps the arguments and exit status railpack expects.
func miseShim(binary string, inject map[string]string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Written by donkey-server. railpack execs this path expecting mise;\n")
	b.WriteString("# it restores the proxy and mirror settings railpack strips, then execs\n")
	b.WriteString("# the real binary. See internal/build/mise.go.\n")
	for _, k := range slices.Sorted(maps.Keys(inject)) {
		fmt.Fprintf(&b, "export %s='%s'\n", k, strings.ReplaceAll(inject[k], "'", `'\''`))
	}
	fmt.Fprintf(&b, "exec %q \"$@\"\n", binary)
	return b.String()
}

func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
