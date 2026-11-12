package cli

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/packages"
)

// packagesPort is where the mirror listens on a developer's machine unless
// told otherwise. Some lockfiles record the address a package came from
// (uv.lock, yarn 1's yarn.lock), so this number is part of what a team
// shares: changing it would rewrite those files for everyone.
const packagesPort = 4874

// packagesCommand brings the installation's package mirror to the
// developer's machine, for networks where the public registries are blocked
// there but reachable from the installation.
func packagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "packages",
		Usage: "install packages through donkey where the public registries are blocked",
		Commands: []*cli.Command{
			packagesRunCommand(),
			packagesProxyCommand(),
		},
	}
}

func packagesPortFlag() cli.Flag {
	return &cli.IntFlag{Name: "port", Value: packagesPort, Usage: "local port of the mirror"}
}

func packagesRunCommand() *cli.Command {
	// Everything from the command's name on is the command's own: its flags
	// are not donkey's to parse.
	command := 1
	return &cli.Command{
		Name:         "run",
		Usage:        "run a command with its package manager pointed at donkey's mirror, e.g. donkey packages run npm install",
		ArgsUsage:    "COMMAND [ARGS...]",
		Flags:        []cli.Flag{packagesPortFlag()},
		StopOnNthArg: &command,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			args := cmd.Args().Slice()
			if len(args) == 0 {
				return fmt.Errorf("tell donkey what to run — e.g. `donkey packages run npm install`")
			}
			mirror, err := openPackagesMirror(ctx, s, int(cmd.Int("port")), false)
			if err != nil {
				return err
			}
			defer mirror.Close()

			settings, err := writePackagesSettings(mirror.URL)
			if err != nil {
				return err
			}
			args = packagesArgs(args, settings)
			child := exec.Command(args[0], args[1:]...)
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			child.Env = mergeEnv(os.Environ(), packagesEnv(mirror.URL, settings))

			// Ctrl-C reaches the command by itself — the terminal sends it to
			// both of us — and donkey has to outlive it to report how it
			// ended. A termination is addressed to donkey alone and is
			// passed on.
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(signals)
			if err := child.Start(); err != nil {
				return fmt.Errorf("donkey couldn't start %s: %w", args[0], err)
			}
			go func() {
				for sig := range signals {
					if sig != os.Interrupt {
						_ = child.Process.Signal(sig)
					}
				}
			}()
			err = child.Wait()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				// The command has said what went wrong; donkey only passes
				// its verdict on.
				return cli.Exit("", max(exit.ExitCode(), 1))
			}
			return err
		},
	}
}

func packagesProxyCommand() *cli.Command {
	return &cli.Command{
		Name:  "proxy",
		Usage: "keep the mirror open on this machine, for an IDE or tools that don't start from a terminal",
		Flags: []cli.Flag{packagesPortFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			mirror, err := openPackagesMirror(ctx, s, int(cmd.Int("port")), true)
			if err != nil {
				return err
			}
			defer mirror.Close()

			u := s.UI
			u.Success("The package mirror of %s is on this machine:", s.Server.URL)
			for _, registry := range [][2]string{
				{"npm", "/npm/"}, {"PyPI", "/pypi/"}, {"Go", "/go/"}, {"Maven", "/maven/"}, {"NuGet", "/nuget/index.json"},
			} {
				u.Printf("    %-6s %s", registry[0], mirror.URL+registry[1])
			}
			u.Info("%s uses it for one command; an IDE or a tool's own settings can name these addresses.", u.Bold("donkey packages run <command>"))
			u.Info("The line lives only while this command runs — Ctrl-C closes it.")
			select {
			case err := <-mirror.failed:
				return err
			case <-ctx.Done():
				u.Printf("Mirror closed.")
				return nil
			}
		},
	}
}

// packagesMirror is the installation's package mirror at a local address.
type packagesMirror struct {
	URL    string
	failed chan error
	close  func()
}

func (m *packagesMirror) Close() {
	if m.close != nil {
		m.close()
	}
}

// openPackagesMirror serves the mirror on 127.0.0.1:port. Package managers
// talk to it without credentials; each request leaves as the signed-in user.
//
// A `run` that finds the port taken uses what is there only if it is this
// user's own `donkey packages proxy` for the same server (see packagesProof).
// Next to anything else — another run, which may end first, or a different
// program — it takes a free port instead: the command works either way, only
// a lockfile that records addresses would name the odd port.
func openPackagesMirror(ctx context.Context, s *session, port int, lasting bool) (*packagesMirror, error) {
	api, err := s.api()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		own := ownPackagesProxy(ctx, port, s.Server.URL)
		switch {
		case own && lasting:
			return nil, fmt.Errorf("the package mirror is already open on port %d — `donkey packages run <command>` uses it", port)
		case own:
			return &packagesMirror{URL: fmt.Sprintf("http://127.0.0.1:%d", port)}, nil
		case lasting:
			return nil, fmt.Errorf("port %d is taken on this machine — pick another with --port: %w", port, err)
		}
		if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
		s.UI.Warn("Port %d is taken on this machine, so this run uses %d. A lockfile that records where packages came from (uv.lock) would name that port.",
			port, listener.Addr().(*net.TCPAddr).Port)
	}
	mirror, err := servePackagesMirror(ctx, api, listener, s.Server.URL, lasting)
	if err != nil {
		listener.Close()
	}
	return mirror, err
}

func servePackagesMirror(ctx context.Context, api *apiclient.Client, listener net.Listener, server string, lasting bool) (*packagesMirror, error) {
	if err := api.Packages(ctx); err != nil {
		return nil, err
	}
	proxy, err := api.PackagesProxy()
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	var secret []byte
	if lasting {
		// What a later `run` holds this listener to before it trusts it.
		secret = []byte(rand.Text())
		dir, err := packagesDir(listener.Addr().(*net.TCPAddr).Port)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, packagesSecretFile), secret, 0o600); err != nil {
			return nil, err
		}
	}
	srv := &http.Server{Handler: packagesLocalHandler(proxy, address, server, secret), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	return &packagesMirror{URL: "http://" + address, failed: failed, close: func() { srv.Close() }}, nil
}

// packagesSecretFile holds, readable by its user alone, the secret of the
// `donkey packages proxy` that last opened a port.
const packagesSecretFile = "proxy"

// packagesProof is how a `donkey packages proxy` shows a `run` that it is
// what it seems: it answers a challenge with a MAC under a secret kept where
// only its user can read. Another program on the port — on a machine people
// share, anyone can take it first — cannot, and is not handed a package
// manager to feed. The server is part of it, so a proxy signed in elsewhere
// is not taken for this one.
func packagesProof(secret []byte, challenge, server string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(challenge + "\n" + server))
	return hex.EncodeToString(mac.Sum(nil))
}

// ownPackagesProxy reports whether this user's `donkey packages proxy` for
// the server holds a local port.
func ownPackagesProxy(ctx context.Context, port int, server string) bool {
	dir, err := packagesDir(port)
	if err != nil {
		return false
	}
	secret, err := os.ReadFile(filepath.Join(dir, packagesSecretFile))
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	challenge := rand.Text()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/?challenge=%s", port, challenge), nil)
	if err != nil {
		return false
	}
	// Straight to the port: a forward proxy from the environment has no
	// business with a loopback address.
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	proof, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return hmac.Equal(proof, []byte(packagesProof(secret, challenge, server)))
}

// packagesLocalHandler is what listens on the developer's machine. It reads
// only, like the mirror behind it, and answers only a client that addressed
// it by its local name: a web page open in the developer's browser can aim
// requests at a local port under a name of its own, and would otherwise read
// the installation's packages with the developer's sign-in. A proxy that
// stays (it has a secret) also answers challenges at its root.
func packagesLocalHandler(mirror http.Handler, address, server string, secret []byte) http.Handler {
	_, port, _ := net.SplitHostPort(address)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != address && r.Host != "localhost:"+port {
			http.Error(w, "this is donkey's package mirror for this machine — reach it as "+address, http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the package mirror is read-only", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/" && secret != nil {
			fmt.Fprint(w, packagesProof(secret, r.URL.Query().Get("challenge"), server))
			return
		}
		mirror.ServeHTTP(w, r)
	})
}

// packagesDir is where donkey keeps what belongs to the mirror on a local
// port: in the user's own cache folder, never a place others can write to —
// one of the files in it is a script Gradle runs.
func packagesDir(port int) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("donkey couldn't find your cache folder: %w", err)
	}
	dir := filepath.Join(root, "donkey", "packages", strconv.Itoa(port))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// packagesSettings are the files Maven, Gradle and NuGet take a mirror from.
// None of them reads one from the environment.
type packagesSettings struct {
	Maven  string // settings.xml
	Gradle string // an init script
	NuGet  string // NuGet.Config
}

// writePackagesSettings writes those files for a mirror and says where: in
// packagesDir, so the same mirror always means the same paths and contents.
// There is nothing to clean up behind a Gradle daemon that outlives the
// command, and nothing for a build cache to see as a change.
func writePackagesSettings(mirror string) (packagesSettings, error) {
	port, _ := strconv.Atoi(mirror[strings.LastIndex(mirror, ":")+1:])
	dir, err := packagesDir(port)
	if err != nil {
		return packagesSettings{}, err
	}
	settings := packagesSettings{
		Maven:  filepath.Join(dir, "settings.xml"),
		Gradle: filepath.Join(dir, "init.gradle"),
		NuGet:  filepath.Join(dir, "NuGet.Config"),
	}
	for path, content := range map[string]string{
		settings.Maven:  mavenSettings,
		settings.Gradle: gradleInitScript,
		settings.NuGet:  nugetConfig,
	} {
		content = strings.NewReplacer(
			"{{maven}}", mirror+"/maven/",
			"{{plugins}}", mirror+packages.GradlePluginPortalPath,
			"{{nuget}}", mirror+"/nuget/index.json",
		).Replace(content)
		// Every run of the same mirror wants the same bytes, so after the
		// first there is nothing to write — and nothing a tool that is
		// reading the file just now could trip over.
		if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
			continue
		}
		// Renamed into place, so the file is never read half-written.
		partial, err := os.CreateTemp(dir, ".partial-*")
		if err != nil {
			return packagesSettings{}, err
		}
		_, err = partial.WriteString(content)
		if closeErr := partial.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(partial.Name(), path)
		}
		if err != nil {
			os.Remove(partial.Name())
			return packagesSettings{}, fmt.Errorf("donkey couldn't write %s: %w", path, err)
		}
	}
	return settings, nil
}

// Every repository outside this machine is read from the mirror. Offered as
// global settings, so the developer's own settings.xml still applies — and
// wins where it names a mirror of its own.
const mavenSettings = `<?xml version="1.0" encoding="UTF-8"?>
<!-- Written by "donkey packages run": Maven reads its repositories through donkey's package mirror. -->
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <mirrors>
    <mirror>
      <id>donkey-packages</id>
      <mirrorOf>external:*</mirrorOf>
      <url>{{maven}}</url>
    </mirror>
  </mirrors>
</settings>
`

// Gradle names repositories in the build itself, so the script rewrites what
// each build declares: the plugin portal to the portal behind the mirror,
// which has the plugin markers Maven Central does not carry, and every other
// repository to the mirror. A build that names no plugin repository asks the
// portal by default; it is given both, the mirror first.
const gradleInitScript = `// Written by "donkey packages run": Gradle reads the repositories a build
// declares, and looks plugins up, through donkey's package mirror.
def mirror = '{{maven}}'
def plugins = '{{plugins}}'
def local = ['127.0.0.1', 'localhost']
def useMirror = { repositories ->
    repositories.withType(org.gradle.api.artifacts.repositories.MavenArtifactRepository).configureEach { repository ->
        if (repository.url.scheme in ['http', 'https'] && !(repository.url.host in local)) {
            repository.url = repository.url.host == 'plugins.gradle.org' ? plugins : mirror
            repository.allowInsecureProtocol = true
        }
    }
}
beforeSettings { settings ->
    settings.pluginManagement.repositories.maven { name = 'donkeyPackages'; url = mirror; allowInsecureProtocol = true }
    settings.pluginManagement.repositories.maven { name = 'donkeyPluginPortal'; url = plugins; allowInsecureProtocol = true }
}
settingsEvaluated { settings ->
    useMirror(settings.pluginManagement.repositories)
    if (settings.metaClass.respondsTo(settings, 'getDependencyResolutionManagement')) {
        useMirror(settings.dependencyResolutionManagement.repositories)
    }
}
allprojects { project ->
    useMirror(project.buildscript.repositories)
    useMirror(project.repositories)
}
`

// NuGet refuses a plain-HTTP source, this machine included, unless the
// source's own entry allows it — which only a config file can say.
const nugetConfig = `<?xml version="1.0" encoding="utf-8"?>
<!-- Written by "donkey packages run": NuGet restores through donkey's package mirror. -->
<configuration>
  <packageSources>
    <clear />
    <add key="donkey-packages" value="{{nuget}}" protocolVersion="3" allowInsecureConnections="true" />
  </packageSources>
</configuration>
`

// packagesArgs adds what Maven and Gradle take on their command line only.
// Gradle has no other way in. Maven reads MAVEN_ARGS from 3.9 on, and older
// ones are still what many machines have; naming the settings twice is
// harmless. Appended, so settings the developer names themselves come first
// and win.
func packagesArgs(args []string, settings packagesSettings) []string {
	name := strings.ToLower(filepath.Base(args[0]))
	for _, extension := range []string{".cmd", ".bat", ".exe"} {
		name = strings.TrimSuffix(name, extension)
	}
	switch name {
	case "mvn", "mvnw", "mvnd":
		return append(slices.Clone(args), "-gs", settings.Maven)
	case "gradle", "gradlew":
		return append(slices.Clone(args), "--init-script", settings.Gradle)
	}
	return args
}

// packagesEnv points the package managers of this machine at the mirror: what
// packages.ManagerEnv says for every place people work, plus what belongs to
// a mirror opened by this command — the settings files it wrote, and the
// environment it is started from.
func packagesEnv(mirror string, settings packagesSettings) map[string]string {
	env := packages.ManagerEnv(mirror)
	// MSBuild reads its properties from the environment, so this reaches
	// every restore: dotnet restore, and the one inside build, run, test.
	env["RestoreConfigFile"] = settings.NuGet
	// Maven 3.9 and later, also where mvn is started by a script or a build
	// tool rather than named on donkey's command line. The mvn script splits
	// this on spaces and cmd.exe honors quotes, so a path with a space in it
	// can only be said on Windows.
	if maven := settings.Maven; !strings.Contains(maven, " ") {
		env["MAVEN_ARGS"] = strings.TrimSpace(os.Getenv("MAVEN_ARGS") + " -gs " + maven)
	} else if runtime.GOOS == "windows" {
		env["MAVEN_ARGS"] = strings.TrimSpace(os.Getenv("MAVEN_ARGS") + ` -gs "` + maven + `"`)
	}
	// A machine that cannot reach the registries cannot reach Go's checksum
	// database either. go.sum still pins what was downloaded; a database the
	// developer configured themselves is left alone.
	if os.Getenv("GOSUMDB") == "" {
		env["GOSUMDB"] = "off"
	}
	// Where the registries are blocked there is usually a forward proxy in
	// the environment, and most package managers would hand it the requests
	// meant for this machine.
	direct := []string{"127.0.0.1", "localhost"}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		for host := range strings.SplitSeq(os.Getenv(name), ",") {
			if host = strings.TrimSpace(host); host != "" && !slices.Contains(direct, host) {
				direct = append(direct, host)
			}
		}
	}
	env["NO_PROXY"], env["no_proxy"] = strings.Join(direct, ","), strings.Join(direct, ",")
	return env
}

// mergeEnv lays env over base. Names are matched without regard to case:
// npm reads npm_config_registry in any spelling, and a machine that already
// sets one must not end up with two.
func mergeEnv(base []string, env map[string]string) []string {
	overridden := map[string]bool{}
	for name := range env {
		overridden[strings.ToUpper(name)] = true
	}
	merged := make([]string, 0, len(base)+len(env))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if !overridden[strings.ToUpper(name)] {
			merged = append(merged, entry)
		}
	}
	for name, value := range env {
		merged = append(merged, name+"="+value)
	}
	return merged
}
