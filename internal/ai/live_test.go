package ai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These run the assistant against a real model, which is the only way to find
// out whether a prompt change helped. Pointing OPENAI_BASE_URL at an endpoint
// is what opts in — there is no separate switch, because setting one without
// the other never means anything. Without it they skip, so an ordinary
// `go test ./...` needs neither a key nor a network.
//
//	OPENAI_BASE_URL=http://localhost:8080/v1 OPENAI_API_KEY=… \
//	DONKEY_MODEL=claude-sonnet-5 go test ./internal/ai -run TestLive -v
//
// `task builder:write` is the same thing with the flags already set.
//
// What they assert is deliberately narrow: the failures that are cheap to
// check and expensive to ship. Whether a Dockerfile is *good* is answered by
// building it, which is what the builder's own verification step is for.
func liveAssistant(t *testing.T) Assistant {
	t.Helper()
	if os.Getenv("OPENAI_BASE_URL") == "" {
		t.Skip("set OPENAI_BASE_URL (and OPENAI_API_KEY) to run the live tests")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		// Local gateways generally do not check it, and a missing key would
		// otherwise fail in New() rather than saying anything useful.
		t.Setenv("OPENAI_API_KEY", "local")
	}
	model := os.Getenv("DONKEY_MODEL")
	if model == "" {
		model = "claude-sonnet-5"
	}
	a, err := New(model)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// repoRoot walks up to the module root so tests can reach testdata/.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root")
	return ""
}

// liveCase is one app and what its Dockerfile has to get right.
type liveCase struct {
	// dir is relative to testdata/.
	dir  string
	port int
	// wants are strings that must appear in the Dockerfile, case-insensitively.
	wants []string
	// avoids are strings that must not — each one a failure that ships.
	avoids []string
	// mustExplore marks apps the summary cannot settle on its own: a monorepo,
	// a framework whose output directory lives in a config file. Answering
	// those without opening anything is a guess, however good it looks.
	//
	// Small apps are deliberately not marked. A single-file Flask app is fully
	// described by the summary, and spending tool calls to re-read what is
	// already in the prompt would be waste, not diligence.
	mustExplore bool
}

func TestLiveWriteDockerfile(t *testing.T) {
	assistant := liveAssistant(t)
	root := repoRoot(t)

	cases := map[string]liveCase{
		"flask": {
			dir:  "flask-beginner",
			port: 8080,
			// A bare `flask run` or `app.run()` binds loopback and serves one
			// request at a time. Both start, so only reading catches them.
			avoids: []string{"flask run", "127.0.0.1"},
		},
		"fastapi": {
			dir:    "fastapi-sample",
			port:   8080,
			wants:  []string{"uvicorn"},
			avoids: []string{"--reload", "127.0.0.1"},
		},
		"django": {
			dir:  "django-blog",
			port: 8080,
			// runserver is a development server. It is also what every Django
			// tutorial says to type, which is why it needs asserting.
			avoids: []string{"runserver", "127.0.0.1"},
			// The WSGI module path is <project>.wsgi, and the project name is
			// only discoverable by looking at the folder layout.
			mustExplore: true,
		},
		"express": {
			dir:  "express-api",
			port: 8080,
		},
		"nextjs": {
			dir:  "nextjs-app",
			port: 8080,
		},
		"monorepo root": {
			dir:  "monorepo-root",
			port: 8080,
			// Which package is the app is not answerable from the root
			// manifest, so this one has to go and look.
			mustExplore: true,
		},
		"static site": {
			dir:  "static-site",
			port: 8080,
		},
		"streamlit": {
			dir:  "streamlit-report",
			port: 8080,
			// Streamlit binds loopback unless told otherwise, and its default
			// port is 8501 whatever PORT says.
			wants: []string{"streamlit"},
		},
		"sqlite notes": {
			dir:  "sqlite-notes",
			port: 8080,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(root, "testdata", tc.dir)
			if _, err := os.Stat(dir); err != nil {
				t.Skipf("%s is not in testdata", tc.dir)
			}
			summary, err := Summarize(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			res, err := assistant.WriteDockerfile(ctx, &DockerfileInput{
				Summary: summary,
				Root:    dir,
				Port:    tc.port,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("read %d file(s): %v", len(res.FilesRead), res.FilesRead)
			t.Logf("notes: %s", res.Notes)
			t.Logf("\n%s", res.Dockerfile)

			df := strings.ToLower(res.Dockerfile)
			// Every Dockerfile has to satisfy these, whatever the app is.
			if !strings.Contains(df, "cmd") && !strings.Contains(df, "entrypoint") {
				t.Error("no ENTRYPOINT and no CMD — the image cannot start")
			}
			if strings.Contains(df, ":latest") {
				t.Error("a :latest base image is not a pinned version")
			}
			if strings.Contains(df, "healthcheck") {
				t.Error("donkey adds the health check; the Dockerfile must not")
			}
			for _, w := range tc.wants {
				if !strings.Contains(df, strings.ToLower(w)) {
					t.Errorf("expected %q in the Dockerfile", w)
				}
			}
			for _, a := range tc.avoids {
				if strings.Contains(df, strings.ToLower(a)) {
					t.Errorf("did not expect %q in the Dockerfile", a)
				}
			}
			if tc.mustExplore && len(res.FilesRead) == 0 {
				t.Error("this app cannot be settled from the summary — answering without opening a file is a guess")
			}
		})
	}
}

// The static-site sample is the shape most often built wrong: the answer is a
// web server holding a folder, not a Node process serving files.
func TestLiveStaticSiteBecomesAWebServer(t *testing.T) {
	assistant := liveAssistant(t)
	root := repoRoot(t)
	dir := filepath.Join(root, "testdata", "kitchen-sink-sample", "web")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("sample not present")
	}

	// donkey's own scan should have called this before the model sees it.
	sig := Scan(dir)
	if sig.Static == nil {
		t.Fatalf("the scanner did not recognize a static front end:\n%s", sig.Render())
	}
	t.Logf("scanner: %s → ./%s", sig.Static.Framework, sig.Static.OutputDir)

	summary, err := Summarize(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := assistant.WriteDockerfile(ctx, &DockerfileInput{
		Summary: summary, Root: dir, Port: 8080,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", res.Dockerfile)

	df := strings.ToLower(res.Dockerfile)
	if !strings.Contains(df, "caddy") && !strings.Contains(df, "nginx") {
		t.Errorf("a static site should end up on a web server, got:\n%s", res.Dockerfile)
	}
	// A multi-stage build is the point: the final image must not carry the
	// toolchain that produced the files.
	if !strings.Contains(df, "--from=") {
		t.Error("expected a multi-stage build that leaves the build toolchain behind")
	}
}

// A failure the model is shown must actually change what it writes. This is
// the loop the whole builder rests on, so it is worth proving rather than
// assuming.
func TestLiveRetryUsesTheFailure(t *testing.T) {
	assistant := liveAssistant(t)
	root := repoRoot(t)
	dir := filepath.Join(root, "testdata", "flask-beginner")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("sample not present")
	}
	summary, err := Summarize(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := assistant.WriteDockerfile(ctx, &DockerfileInput{
		Summary: summary, Root: dir, Port: 8080,
		Attempts: []DockerfileAttempt{{
			Dockerfile: "FROM python:3.12-slim\nWORKDIR /app\nCOPY . .\n" +
				"RUN pip install -r requirements.txt\nCMD [\"python\", \"app.py\"]\n",
			Failure: "the image was built, but it doesn't start: " +
				"[verify] the app exited instead of starting up:\n" +
				"Traceback (most recent call last):\n" +
				"  File \"/app/app.py\", line 1, in <module>\n" +
				"    from flask import Flask\n" +
				"ModuleNotFoundError: No module named 'flask'\n",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read %v", res.FilesRead)
	t.Logf("notes: %s", res.Notes)
	t.Logf("\n%s", res.Dockerfile)

	// The point is not which fix it picks, only that it engaged with the
	// failure rather than re-emitting the same file.
	if strings.Contains(res.Dockerfile, `RUN pip install -r requirements.txt`) &&
		!strings.Contains(res.Dockerfile, "--no-cache-dir") &&
		res.Notes == "" {
		t.Error("the retry looks identical to the attempt that failed")
	}
}
