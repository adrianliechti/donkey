package packages

import (
	"encoding/base64"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// A lockfile pins what an app depends on, and many also record where each
// package was downloaded from. That address is the one thing in them that is
// about a machine rather than about the app: the public registry on a laptop
// with internet, an enterprise registry on one inside the company,
// `donkey packages` on one without either. A build reaches none of them for
// certain, only this mirror — and its own registry setting already says so
// for everything the lockfile does not pin. So before a build reads a
// lockfile, the host in every registry address is disregarded: the package's
// name and file stay, and the mirror is asked for them. Versions and
// checksums are untouched, which is what makes the result the same packages
// rather than similar ones — a file the mirror serves under that name either
// matches the lockfile's checksum or fails the install.
//
// One kind of address is kept: a registry the app itself assigns to
// particular packages — a scoped registry in .npmrc, a named index in
// pyproject.toml. The build's registry setting does not replace those
// either, so the lockfile and the configuration go on agreeing.

// publicPyPIFiles is where pypi.org keeps the files its index links to.
const publicPyPIFiles = "https://files.pythonhosted.org"

// pypiFileMarker asks the PyPI route for a file by its name alone.
const pypiFileMarker = "-/file/"

var (
	// Where a lockfile records a download — npm's "resolved", yarn 1's
	// resolved, pnpm's tarball, the address slot of an entry in bun.lock —
	// followed by an address in the shape every npm registry uses:
	// <registry>/<name>/-/<file>.tgz, the name with its scope where it has
	// one. The same address elsewhere is a dependency someone wrote as a
	// URL, which the package manager compares with package.json.
	npmAddress = regexp.MustCompile(`("resolved": "|resolved "|tarball: |\["[^"]*", ")` +
		`https?://[^\s"'<>]+?/((?:@[^/\s"'<>%]+(?:/|%2[fF]))?[^/\s"'<>@%][^/\s"'<>]*)/-/([^/\s"'<>#?]+\.tgz)`)
	// uv.lock, as uv writes it: the index a package came from — on the
	// package itself, and again in every dependency entry that has to tell
	// two locked versions of a package apart — and the package's files.
	uvSource = regexp.MustCompile(`(source = \{ registry = ")(https?://[^"]+)(" \})`)
	uvFile   = regexp.MustCompile(`(url = ")(https?://[^"]+)(")`)
	// `donkey packages` on a developer's machine, on whichever port: what
	// follows is this mirror's own path.
	localPyPI = regexp.MustCompile(`^http://(?:127\.0\.0\.1|localhost):\d+/pypi/`)

	// A registry assigned to an npm scope: "@scope:registry=…" in .npmrc,
	// the same quoted in .yarnrc, "@scope" = … under bunfig.toml's scopes.
	scopedRegistry = regexp.MustCompile(`(?m)^\s*["']?@.*?(https?://[^\s"']+)`)
	tomlURL        = regexp.MustCompile(`^url\s*=\s*"(https?://[^"]+)"`)
	tomlDefault    = regexp.MustCompile(`^default\s*=\s*true`)
)

// lockfileRewrites are the lockfiles that record addresses, by file name.
var lockfileRewrites = map[string]func(content, mirror string, declared []string) string{
	"package-lock.json":   rewriteNPMLockfile,
	"npm-shrinkwrap.json": rewriteNPMLockfile,
	"yarn.lock":           rewriteNPMLockfile,
	"pnpm-lock.yaml":      rewriteNPMLockfile,
	"bun.lock":            rewriteNPMLockfile,
	"uv.lock":             rewritePyPILockfile,
}

// IsLockfile reports whether a file of this name may record package
// addresses RewriteLockfile translates.
func IsLockfile(name string) bool {
	return lockfileRewrites[path.Base(name)] != nil
}

// RewriteLockfile points the registry addresses in a lockfile at the mirror
// (its address as a build reaches it, e.g. http://donkey-packages), whatever
// host they name. Addresses under a declared registry (see
// DeclaredRegistries) stay as written, and so does everything that is not a
// registry download: a dependency from git, from a plain URL, from a path.
func RewriteLockfile(name string, content []byte, mirror string, declared []string) []byte {
	rewrite := lockfileRewrites[path.Base(name)]
	if rewrite == nil {
		return content
	}
	rewritten := rewrite(string(content), strings.TrimRight(mirror, "/"), declared)
	if rewritten == string(content) {
		return content
	}
	return []byte(rewritten)
}

func rewriteNPMLockfile(content, mirror string, declared []string) string {
	return npmAddress.ReplaceAllStringFunc(content, func(entry string) string {
		match := npmAddress.FindStringSubmatch(entry)
		if under(entry[len(match[1]):], declared) {
			return entry
		}
		// Some registries write a scoped name as one path segment.
		name := strings.NewReplacer("%2f", "/", "%2F", "/").Replace(match[2])
		return match[1] + mirror + "/npm/" + name + "/-/" + match[3]
	})
}

func rewritePyPILockfile(content, mirror string, declared []string) string {
	index := mirror + "/pypi/"
	lines := strings.Split(content, "\n")
	// Whether the package being read came from an index the build replaces.
	// Its files follow its source line.
	replaced := false
	for i, line := range lines {
		line, carriageReturn := strings.CutSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "[[package]]"):
			replaced = false
		case strings.HasPrefix(line, "source = "):
			own := uvSource.FindStringSubmatch(line)
			replaced = own != nil && !under(own[2], declared)
		case replaced:
			line = uvFile.ReplaceAllStringFunc(line, func(entry string) string {
				file := uvFile.FindStringSubmatch(entry)
				return file[1] + pypiFileAddress(file[2], index) + file[3]
			})
		}
		line = uvSource.ReplaceAllStringFunc(line, func(entry string) string {
			source := uvSource.FindStringSubmatch(entry)
			if under(source[2], declared) {
				return entry
			}
			return source[1] + index + source[3]
		})
		if carriageReturn {
			line += "\r"
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// pypiFileAddress is where the mirror serves a file a lockfile names. Two
// addresses have an exact counterpart on the mirror and keep their path: the
// public file host, in the form the mirror itself gives it in an index, and
// `donkey packages`, which is this mirror. Any other is asked for by the
// file's name, which the mirror looks up in its upstream's index — a page of
// megabytes for a large project, so the exact forms are worth having.
func pypiFileAddress(address, index string) string {
	if rest, ok := strings.CutPrefix(address, publicPyPIFiles+"/"); ok {
		return index + resourceMarker + base64.RawURLEncoding.EncodeToString([]byte(publicPyPIFiles)) + "/" + rest
	}
	if local := localPyPI.FindString(address); local != "" {
		return index + address[len(local):]
	}
	if parsed, err := url.Parse(address); err == nil {
		if name := path.Base(parsed.EscapedPath()); name != "." && name != "/" {
			return index + pypiFileMarker + name
		}
	}
	return address
}

// under reports whether an address lies under one of the registries.
func under(address string, registries []string) bool {
	for _, registry := range registries {
		registry = strings.TrimRight(registry, "/")
		if rest, ok := strings.CutPrefix(address, registry); ok && registry != "" && (rest == "" || rest[0] == '/') {
			return true
		}
	}
	return false
}

// IsRegistryConfig reports whether a file of this name may assign
// registries to particular packages.
func IsRegistryConfig(name string) bool {
	switch path.Base(name) {
	case ".npmrc", ".yarnrc", "bunfig.toml", "pyproject.toml", "uv.toml":
		return true
	}
	return false
}

// DeclaredRegistries reads from one of an app's configuration files the
// registries it assigns to particular packages: a registry for an npm scope,
// and uv's indexes other than the default one. A build's own registry
// setting replaces only the default registry, so packages from these still
// come from where the app says.
func DeclaredRegistries(name string, content []byte) []string {
	var declared []string
	switch path.Base(name) {
	case ".npmrc", ".yarnrc", "bunfig.toml":
		for _, match := range scopedRegistry.FindAllSubmatch(content, -1) {
			declared = append(declared, string(match[1]))
		}
	case "pyproject.toml":
		declared = uvIndexes(string(content), "[[tool.uv.index]]")
	case "uv.toml":
		declared = uvIndexes(string(content), "[[index]]")
	}
	return declared
}

// uvIndexes reads the indexes a uv configuration declares table by table,
// leaving out the one marked as the default.
func uvIndexes(content, table string) []string {
	var declared []string
	address, isDefault := "", false
	keep := func() {
		if address != "" && !isDefault {
			declared = append(declared, address)
		}
		address, isDefault = "", false
	}
	inside := false
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			keep()
			inside = line == table
			continue
		}
		if !inside {
			continue
		}
		if match := tomlURL.FindStringSubmatch(line); match != nil {
			address = match[1]
		}
		if tomlDefault.MatchString(line) {
			isDefault = true
		}
	}
	keep()
	return declared
}
