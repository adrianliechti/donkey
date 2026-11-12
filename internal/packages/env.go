package packages

import "net/url"

// ManagerEnv points the package managers of a place where people work — a
// developer's own computer, a personal machine — at a mirror, by the names
// the tools read themselves. It is what a build is given (see
// build.BuildKitBuilder), plus what only matters where a lockfile is
// written: a build installs from lockfiles, a person writes them.
//
// mirror is the mirror's address without a trailing slash, as the package
// managers reach it. The tools that take a mirror from a file rather than
// from the environment (Maven 3, Gradle, NuGet) are not covered here.
func ManagerEnv(mirror string) map[string]string {
	host := mirror
	if u, err := url.Parse(mirror); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return map[string]string{
		// npm, pnpm, yarn 1 and bun.
		"NPM_CONFIG_REGISTRY": mirror + "/npm/",
		// The mirror hands out tarball links at its own address, and npm
		// would write them into package-lock.json, where the next machine —
		// or the build, which knows the mirror under another name — cannot
		// follow them. Without them the lockfile still pins every version
		// and its checksum, and each install asks its own registry.
		"NPM_CONFIG_OMIT_LOCKFILE_REGISTRY_RESOLVED": "true",
		// yarn 2 and later, which also wants to be told that plain HTTP to
		// the mirror is meant.
		"YARN_NPM_REGISTRY_SERVER":   mirror + "/npm/",
		"YARN_UNSAFE_HTTP_WHITELIST": host,
		"PIP_INDEX_URL":              mirror + "/pypi/",
		// Not UV_DEFAULT_INDEX: `uv add` writes that one into pyproject.toml.
		"UV_INDEX_URL": mirror + "/pypi/",
		"GOPROXY":      mirror + "/go/",
		// Maven 4, and the Maven wrapper's own download — which appends its
		// path with a slash of its own.
		"MAVEN_REPO_CENTRAL": mirror + "/maven/",
		"MVNW_REPOURL":       mirror + "/maven",
	}
}
