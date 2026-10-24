package detect

import "strings"

// Stack names what an app is written in, read off the base images of the
// Dockerfile it was built from — the one fact every build has, whether the
// app brought the recipe or the assistant wrote it. The answer is a short
// key the console turns into an icon ("python", "go", "node", …), or "" when
// no image says.
//
// A language beats a web server: a site built with node and served by
// nginx is a node app, a Go binary on distroless is a Go app. Among
// languages the first stage wins, since builds start from the toolchain.
func Stack(baseImages []string) string {
	web := ""
	for _, image := range baseImages {
		stack := imageStack(image)
		if stack == StackStatic {
			if web == "" {
				web = stack
			}
			continue
		}
		if stack != "" {
			return stack
		}
	}
	return web
}

// StackStatic is a site served as files, by nginx, caddy or httpd.
const StackStatic = "static"

// stackImages maps an image's repository, without registry or tag, to a
// stack. Official images are matched by name, others by their full path.
var stackImages = map[string]string{
	"python":       "python",
	"pypy":         "python",
	"astral-sh/uv": "python",

	"node":          "node",
	"oven/bun":      "bun",
	"denoland/deno": "deno",

	"golang": "go",

	"eclipse-temurin":     "java",
	"openjdk":             "java",
	"amazoncorretto":      "java",
	"ibm-semeru-runtimes": "java",
	"sapmachine":          "java",
	"maven":               "java",
	"gradle":              "java",

	"ruby":     "ruby",
	"php":      "php",
	"composer": "php",
	"rust":     "rust",

	"dotnet/sdk":          "dotnet",
	"dotnet/aspnet":       "dotnet",
	"dotnet/runtime":      "dotnet",
	"dotnet/runtime-deps": "dotnet",

	"elixir":       "elixir",
	"hexpm/elixir": "elixir",

	"nginx":                       StackStatic,
	"nginxinc/nginx-unprivileged": StackStatic,
	"caddy":                       StackStatic,
	"httpd":                       StackStatic,
}

func imageStack(image string) string {
	repo := strings.ToLower(image)
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo = repo[:colon]
	}
	// A first segment with a dot or port is a registry host.
	if first, rest, ok := strings.Cut(repo, "/"); ok && strings.ContainsAny(first, ".:") {
		repo = rest
	}
	repo = strings.TrimPrefix(repo, "library/")
	if stack, ok := stackImages[repo]; ok {
		return stack
	}
	// Distroless images name their language in the image: distroless/java21.
	if strings.HasPrefix(repo, "distroless/") {
		for _, lang := range []string{"java", "python", "nodejs"} {
			if strings.HasPrefix(strings.TrimPrefix(repo, "distroless/"), lang) {
				if lang == "nodejs" {
					return "node"
				}
				return lang
			}
		}
	}
	return ""
}
