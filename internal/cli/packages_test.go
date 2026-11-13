package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	urfavecli "github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/server"
	"github.com/adrianliechti/donkey/internal/ui"
)

// packagesAPI stands in for donkey-server: it offers the mirror and answers
// every package request with the path it was asked for.
func packagesAPI(t *testing.T) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sealed-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"not signed in — run donkey login"}`)
			return
		}
		if r.URL.Path == "/v1/packages/" {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		fmt.Fprintf(w, "%s as %s", r.URL.Path, r.Header.Get(server.PackagesAddressHeader))
	}))
	t.Cleanup(api.Close)
	return api
}

func packagesSession(api string) *session {
	return &session{UI: ui.New(false, false), Server: config.ServerConfig{URL: api, Token: "sealed-token"}}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body)
}

// What a package manager asks the local address reaches donkey-server as
// the signed-in user, under the address it was asked at.
func TestPackagesMirrorCarriesRequestsToTheServer(t *testing.T) {
	t.Setenv("DONKEY_TOKEN", "")
	api := packagesAPI(t)
	port := freePort(t)
	mirror, err := openPackagesMirror(context.Background(), packagesSession(api.URL), port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()

	address := "127.0.0.1:" + strconv.Itoa(port)
	if mirror.URL != "http://"+address {
		t.Fatalf("mirror at %s, want port %d", mirror.URL, port)
	}
	if body := get(t, mirror.URL+"/npm/left-pad"); body != "/v1/packages/npm/left-pad as "+address {
		t.Errorf("answer %q", body)
	}
	// A tool configured with the other local name gets its links under it.
	if body := get(t, "http://localhost:"+strconv.Itoa(port)+"/go/rsc.io/quote/@v/list"); body != "/v1/packages/go/rsc.io/quote/@v/list as localhost:"+strconv.Itoa(port) {
		t.Errorf("answer %q", body)
	}
}

// A sign-in that is no good is said before any command starts.
func TestPackagesMirrorChecksTheSignInFirst(t *testing.T) {
	t.Setenv("DONKEY_TOKEN", "")
	api := packagesAPI(t)
	s := packagesSession(api.URL)
	s.Server.Token = "expired"
	if _, err := openPackagesMirror(context.Background(), s, freePort(t), false); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("error %v, want the sign-in named", err)
	}
}

// The local address answers package managers on this machine and nothing
// else: not a page that reached the port under a name of its own, and not a
// write.
func TestPackagesLocalHandlerRefusesOtherNamesAndWrites(t *testing.T) {
	reached := 0
	secret := []byte("only-its-user-can-read-this")
	handler := packagesLocalHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }),
		"127.0.0.1:4874", "https://donkey.example.com", secret)
	call := func(method, host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://"+host+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, host := range []string{"evil.example.com:4874", "127.0.0.1:4875", "127.0.0.1"} {
		if rec := call(http.MethodGet, host, "/npm/left-pad"); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %s: status %d, want 421", host, rec.Code)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		if rec := call(method, "127.0.0.1:4874", "/npm/left-pad"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, rec.Code)
		}
	}
	if reached != 0 {
		t.Fatalf("%d refused requests travelled on", reached)
	}
	// A proxy that stays answers a challenge with what only it can compute,
	// and never with the secret itself.
	proof := call(http.MethodGet, "127.0.0.1:4874", "/?challenge=abc").Body.String()
	if proof != packagesProof(secret, "abc", "https://donkey.example.com") || strings.Contains(proof, string(secret)) {
		t.Errorf("answer to a challenge: %q", proof)
	}
	if proof == packagesProof(secret, "abd", "https://donkey.example.com") || proof == packagesProof(secret, "abc", "https://other.example.com") {
		t.Error("the proof does not depend on the challenge and the server")
	}
	for _, host := range []string{"127.0.0.1:4874", "localhost:4874"} {
		call(http.MethodGet, host, "/npm/left-pad")
	}
	if reached != 2 {
		t.Errorf("%d of 2 package requests travelled on", reached)
	}
}

// A run uses a proxy that is already open only when it is this user's own,
// for the same server. Next to anything else it takes a port of its own:
// another run may end first, another server's mirror is not this one's, and
// a program that merely answers like a mirror — on a machine people share,
// anyone can take the port first — must not be handed a package manager.
func TestPackagesRunSharesOnlyItsOwnProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData"))
	t.Setenv("DONKEY_TOKEN", "")
	api := packagesAPI(t)
	ctx := context.Background()

	port := freePort(t)
	proxy, err := openPackagesMirror(ctx, packagesSession(api.URL), port, true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	shared, err := openPackagesMirror(ctx, packagesSession(api.URL), port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if shared.URL != proxy.URL {
		t.Errorf("a run beside its own proxy used %s, want %s", shared.URL, proxy.URL)
	}
	if _, err := openPackagesMirror(ctx, packagesSession(api.URL), port, true); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Errorf("a second proxy: %v, want it to say one is open", err)
	}

	other := packagesAPI(t)
	aside, err := openPackagesMirror(ctx, packagesSession(other.URL), port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer aside.Close()
	if aside.URL == proxy.URL {
		t.Error("a run for another server leaned on this server's proxy")
	}

	runPort := freePort(t)
	first, err := openPackagesMirror(ctx, packagesSession(api.URL), runPort, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openPackagesMirror(ctx, packagesSession(api.URL), runPort, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.URL == first.URL {
		t.Error("a run leaned on another run's mirror, which closes when that command ends")
	}
	if body := get(t, second.URL+"/npm/left-pad"); !strings.HasPrefix(body, "/v1/packages/npm/left-pad as 127.0.0.1:") {
		t.Errorf("answer %q", body)
	}

	// Someone else holds a port this user's proxy once had: the secret is
	// still on disk, and the impostor answers every challenge — with
	// anything but the proof.
	proxy.Close()
	var impostor net.Listener
	for range 50 {
		if impostor, err = net.Listen("tcp", proxy.URL[len("http://"):]); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Bool
	go http.Serve(impostor, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			served.Store(true)
		}
		fmt.Fprint(w, r.URL.Query().Get("challenge"))
	}))
	defer impostor.Close()
	wary, err := openPackagesMirror(ctx, packagesSession(api.URL), port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer wary.Close()
	if wary.URL == proxy.URL {
		t.Fatal("a run used a listener that could not prove it is this user's proxy")
	}
	if body := get(t, wary.URL+"/npm/left-pad"); !strings.HasPrefix(body, "/v1/packages/npm/left-pad") || served.Load() {
		t.Errorf("answer %q; impostor asked for packages: %v", body, served.Load())
	}
}

func TestPackagesEnv(t *testing.T) {
	t.Setenv("GOSUMDB", "")
	t.Setenv("NO_PROXY", "internal.example.com, localhost")
	t.Setenv("no_proxy", "")
	t.Setenv("MAVEN_ARGS", "-T 4")
	env := packagesEnv("http://127.0.0.1:4874", packagesSettings{Maven: "/cache/settings.xml", NuGet: "/cache/NuGet.Config"})
	for name, want := range map[string]string{
		"NPM_CONFIG_REGISTRY":                        "http://127.0.0.1:4874/npm/",
		"COREPACK_NPM_REGISTRY":                      "http://127.0.0.1:4874/npm",
		"NPM_CONFIG_OMIT_LOCKFILE_REGISTRY_RESOLVED": "true",
		"YARN_NPM_REGISTRY_SERVER":                   "http://127.0.0.1:4874/npm/",
		"PIP_INDEX_URL":                              "http://127.0.0.1:4874/pypi/",
		"UV_INDEX_URL":                               "http://127.0.0.1:4874/pypi/",
		"GOPROXY":                                    "http://127.0.0.1:4874/go/",
		"GOSUMDB":                                    "off",
		// Maven 3.9+ and MSBuild take their settings file from here; what
		// the developer already passes Maven stays.
		"MAVEN_ARGS":        "-T 4 -gs /cache/settings.xml",
		"RestoreConfigFile": "/cache/NuGet.Config",
		// The forward proxy of a restricted network must not get the
		// requests meant for this machine; what was exempt stays exempt.
		"NO_PROXY": "127.0.0.1,localhost,internal.example.com",
		"no_proxy": "127.0.0.1,localhost,internal.example.com",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	// `uv add` writes this one into pyproject.toml.
	if _, set := env["UV_DEFAULT_INDEX"]; set {
		t.Error("UV_DEFAULT_INDEX must stay unset")
	}

	t.Setenv("GOSUMDB", "sum.example.com")
	if value, set := packagesEnv("http://127.0.0.1:4874", packagesSettings{})["GOSUMDB"]; set {
		t.Errorf("a checksum database the developer configured was replaced with %q", value)
	}
}

func TestMergeEnvReplacesAnySpelling(t *testing.T) {
	merged := mergeEnv(
		[]string{"PATH=/bin", "npm_config_registry=https://registry.example.com/", "GOPROXY=direct", "=C:=C:\\"},
		map[string]string{"NPM_CONFIG_REGISTRY": "http://127.0.0.1:4874/npm/", "GOPROXY": "http://127.0.0.1:4874/go/"},
	)
	slices.Sort(merged)
	want := []string{"=C:=C:\\", "GOPROXY=http://127.0.0.1:4874/go/", "NPM_CONFIG_REGISTRY=http://127.0.0.1:4874/npm/", "PATH=/bin"}
	if !slices.Equal(merged, want) {
		t.Errorf("merged = %q, want %q", merged, want)
	}
}

// The whole command: the wrapped program sees the mirror in its
// environment, gets its own flags untouched, and its exit code is donkey's.
func TestPackagesRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	api := packagesAPI(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("DONKEY_TOKEN", "sealed-token")
	out := filepath.Join(t.TempDir(), "out")
	port := strconv.Itoa(freePort(t))

	exited := -1
	previous := urfavecli.OsExiter
	urfavecli.OsExiter = func(code int) { exited = code }
	t.Cleanup(func() { urfavecli.OsExiter = previous })

	script := `printf '%s %s %s' "$NPM_CONFIG_REGISTRY" "$1" "$2" > "$0"; exit 7`
	err := Run(context.Background(),
		[]string{"donkey", "packages", "run", "--port", port, "sh", "-c", script, out, "--save-dev", "--port"},
		Options{Server: api.URL})
	if exited != 7 {
		t.Errorf("exit code %d (error %v), want the command's 7", exited, err)
	}
	got, _ := os.ReadFile(out)
	if want := "http://127.0.0.1:" + port + "/npm/ --save-dev --port"; string(got) != want {
		t.Errorf("the command saw %q, want %q", got, want)
	}

	if err := Run(context.Background(), []string{"donkey", "packages", "run"}, Options{Server: api.URL}); err == nil || !strings.Contains(err.Error(), "what to run") {
		t.Errorf("without a command: %v", err)
	}
}

// Maven, Gradle and NuGet read a mirror from a file. The files name this
// mirror, sit at a path that is the same for every run of it, and a second
// run leaves nothing half-written behind.
func TestPackagesSettingsNameTheMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData"))

	var settings packagesSettings
	for range 2 {
		var err error
		if settings, err = writePackagesSettings("http://127.0.0.1:4874"); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(settings.Maven)
	if !strings.HasPrefix(dir, home) || !strings.HasSuffix(filepath.ToSlash(dir), "donkey/packages/4874") {
		t.Errorf("settings written to %s", dir)
	}
	for path, want := range map[string]string{
		settings.Maven:  "<mirrorOf>external:*</mirrorOf>\n      <url>http://127.0.0.1:4874/maven/</url>",
		settings.NuGet:  `value="http://127.0.0.1:4874/nuget/index.json" protocolVersion="3" allowInsecureConnections="true"`,
		settings.Gradle: "def mirror = 'http://127.0.0.1:4874/maven/'\ndef plugins = 'http://127.0.0.1:4874/maven/-/resource/aHR0cHM6Ly9wbHVnaW5zLmdyYWRsZS5vcmc/m2/'",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), want) || strings.Contains(string(content), "{{") {
			t.Errorf("%s:\n%s\nwant it to contain %s", filepath.Base(path), content, want)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("%d files in %s, want the three settings files", len(entries), dir)
	}

	other, err := writePackagesSettings("http://127.0.0.1:51234")
	if err != nil {
		t.Fatal(err)
	}
	if other.Maven == settings.Maven {
		t.Error("a mirror on another port overwrote this one's settings")
	}
}

// Maven before 3.9 and Gradle take their settings on the command line only.
// They are added behind what the developer wrote, for these tools and no
// other.
func TestPackagesArgs(t *testing.T) {
	settings := packagesSettings{Maven: "/cache/settings.xml", Gradle: "/cache/init.gradle"}
	for _, tc := range []struct{ args, want []string }{
		{[]string{"mvn", "-B", "package"}, []string{"mvn", "-B", "package", "-gs", "/cache/settings.xml"}},
		{[]string{"./mvnw", "verify"}, []string{"./mvnw", "verify", "-gs", "/cache/settings.xml"}},
		{[]string{"MVN.CMD", "verify"}, []string{"MVN.CMD", "verify", "-gs", "/cache/settings.xml"}},
		{[]string{"/opt/gradle/bin/gradle", "build"}, []string{"/opt/gradle/bin/gradle", "build", "--init-script", "/cache/init.gradle"}},
		{[]string{"gradlew.bat", "build"}, []string{"gradlew.bat", "build", "--init-script", "/cache/init.gradle"}},
		{[]string{"npm", "install"}, []string{"npm", "install"}},
		{[]string{"sh", "-c", "mvn package"}, []string{"sh", "-c", "mvn package"}},
	} {
		if got := packagesArgs(tc.args, settings); !slices.Equal(got, tc.want) {
			t.Errorf("packagesArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
