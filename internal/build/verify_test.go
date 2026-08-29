package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

func imageWith(entrypoint, cmd []string) ocispecs.Image {
	var img ocispecs.Image
	img.Config.Entrypoint = entrypoint
	img.Config.Cmd = cmd
	return img
}

// The image's own start command is what gets run — not a guess at one.
func TestStartCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		img  ocispecs.Image
		want string
	}{
		"entrypoint and cmd": {
			imageWith([]string{"python"}, []string{"app.py"}), "'python' 'app.py'",
		},
		"cmd only": {
			imageWith(nil, []string{"node", "server.js"}), "'node' 'server.js'",
		},
		"entrypoint only": {
			imageWith([]string{"/app/server"}, nil), "'/app/server'",
		},
		// An argument with a quote in it must not end the quoting and turn the
		// rest into commands.
		"quoting": {
			imageWith([]string{"sh", "-c", "echo 'hi'"}, nil), `'sh' '-c' 'echo '\''hi'\'''`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := startCommand(tc.img); got != tc.want {
				t.Errorf("startCommand() = %s, want %s", got, tc.want)
			}
		})
	}
}

// An image with nothing to run cannot be started at all, and saying so beats
// whatever a shell would make of an empty command line.
func TestStartCommandWithoutAnyCommand(t *testing.T) {
	got := startCommand(ocispecs.Image{})
	if !strings.Contains(got, "exit 1") || !strings.Contains(got, "ENTRYPOINT") {
		t.Errorf("an image with no command should fail with an explanation, got: %s", got)
	}
}

// The probe is a shell script that has to survive being read by a shell.
// Checking it parses is cheap and catches the quoting mistakes that would
// otherwise only surface inside a build.
func TestVerifyScriptIsValidShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell to check against")
	}
	script := verifyScript(imageWith([]string{"python"}, []string{"app.py"}), VerifySpec{Port: 8080, Seconds: 30})
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	// -n parses without running: the app it would start does not exist here.
	if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("the probe is not valid shell: %v\n%s\n---\n%s", err, out, script)
	}
}

func TestVerifyScriptChecksTheRightThings(t *testing.T) {
	script := verifyScript(imageWith([]string{"python"}, []string{"app.py"}), VerifySpec{Port: 9000, Seconds: 20})
	for _, want := range []string{
		"'python' 'app.py'",    // the image's own command
		"nc -z 127.0.0.1 9000", // the port donkey deploys with
		"kill -0 $app",         // a process that exited is the failure
		"exit 1",               // …and it fails the build
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the probe is missing %q:\n%s", want, script)
		}
	}
	// An app still running but not listening is not a broken image — it is an
	// app waiting for something the sandbox hasn't got. That must not fail.
	tail := script[strings.LastIndex(script, VerifyNeverListened):]
	if !strings.Contains(tail, "exit 0") {
		t.Errorf("an app that is still running should pass:\n%s", script)
	}
}

// donkey's PORT is the contract the app is deployed with, so it is the one
// the check uses — even if the image's own env says something else.
func TestVerifyStateUsesDonkeysPort(t *testing.T) {
	img := imageWith([]string{"server"}, nil)
	img.Config.Env = []string{"PORT=3000", "LANG=C.UTF-8"}
	script := verifyScript(img, VerifySpec{Port: 8080, Seconds: 10})
	if !strings.Contains(script, "nc -z 127.0.0.1 8080") {
		t.Errorf("the probe should wait on donkey's port, not the image's:\n%s", script)
	}
}

func TestImageConfig(t *testing.T) {
	if _, err := imageConfig(nil); err == nil {
		t.Error("a build with no image config should be reported, not verified blindly")
	}
	if _, err := imageConfig([]byte("not json")); err == nil {
		t.Error("an unreadable image config should be reported")
	}
	cfg, err := imageConfig([]byte(`{"config":{"Entrypoint":["/app"],"WorkingDir":"/srv"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Config.Entrypoint) != 1 || cfg.Config.WorkingDir != "/srv" {
		t.Errorf("image config not read: %+v", cfg.Config)
	}
}
