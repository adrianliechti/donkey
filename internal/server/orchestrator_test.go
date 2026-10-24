package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

// donkey-orchestrator is reachable from more than donkey-server: on Docker it
// sits on every space network, next to tenant code. So it takes nothing on
// trust — a call needs the server's credential and a sign-in it verifies
// itself, and neither works without the other.
func TestOrchestratorOnlyTakesRequestsFromTheServer(t *testing.T) {
	otherKey := bytes.Repeat([]byte{9}, 32)
	user := sealedToken(t, testSessionKey, "u1@example.com")
	server := orchestratorToken(testSessionKey)
	for _, c := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"nothing at all", nil, http.StatusUnauthorized},
		// What a pod holding a stolen sign-in could send.
		{"a sign-in alone", map[string]string{"X-Access-Token": user}, http.StatusUnauthorized},
		{"a sign-in as Bearer alone", map[string]string{"Authorization": "Bearer " + user}, http.StatusUnauthorized},
		{"the server's credential alone", map[string]string{orchestratorTokenHeader: server}, http.StatusUnauthorized},
		{"the server's credential and an identity header", map[string]string{
			orchestratorTokenHeader: server, "X-Forwarded-User": "u1", "X-Auth-Request-Email": "u1@example.com",
		}, http.StatusUnauthorized},
		{"a sign-in sealed with another key", map[string]string{
			orchestratorTokenHeader: server, "X-Access-Token": sealedToken(t, otherKey, "u1@example.com"),
		}, http.StatusUnauthorized},
		{"a credential derived from another key", map[string]string{
			orchestratorTokenHeader: orchestratorToken(otherKey), "X-Access-Token": user,
		}, http.StatusUnauthorized},
		// Scoped credentials are not interchangeable: telemetry's query
		// token comes from a different key and purpose.
		{"telemetry's query token", map[string]string{
			orchestratorTokenHeader: telemetry.QueryToken(testSessionKey), "X-Access-Token": user,
		}, http.StatusUnauthorized},
		{"what donkey-server sends", map[string]string{orchestratorTokenHeader: server, "X-Access-Token": user}, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The draining orchestrator answers 503 the moment a deploy is
			// authorized, before it touches anything.
			s := drainingOrchestrator()
			req := httptest.NewRequest(http.MethodPost, "http://donkey-orchestrator/v1/deploys", strings.NewReader("upload"))
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.orchestratorRoutes().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("want %d, got %d — %s", c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// The server relays the orchestrator's answer as it is written: the CLI and
// the console read progress line by line, and must still be able to.
func TestServerRelaysADeployStream(t *testing.T) {
	firstRead := make(chan struct{})
	lines := []string{
		`{"line":"Looking at your app…"}`,
		`{"progress":"#1 [internal] load build definition"}`,
		`{"result":{"ok":true,"app":"web","space":"team"}}`,
	}
	type forwarded struct {
		method, path, query, body, contentType string
	}
	seen := make(chan forwarded, 1)
	orchestrator := newFakeOrchestrator(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- forwarded{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("Content-Type")}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, lines[0]+"\n")
		w.(http.Flusher).Flush()
		// Hold the rest back until the client has read the first line: a
		// relay that buffered would never let it.
		select {
		case <-firstRead:
		case <-time.After(5 * time.Second):
			return
		}
		io.WriteString(w, lines[1]+"\n"+lines[2]+"\n")
	})
	api := httptest.NewServer(apiServer(orchestrator.URL).routes())
	defer api.Close()

	req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/deploys?space=team&dryRun=true&sha=abc", strings.NewReader("the upload"))
	req.Header.Set("Authorization", "Bearer "+sealedToken(t, testSessionKey, "u1@example.com"))
	req.Header.Set("Content-Type", "application/gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("relayed %s %s", res.Status, res.Header.Get("Content-Type"))
	}
	got := <-seen
	if got.method != http.MethodPost || got.path != "/v1/deploys" || got.query != "space=team&dryRun=true&sha=abc" ||
		got.body != "the upload" || got.contentType != "application/gzip" {
		t.Fatalf("the orchestrator received %+v", got)
	}

	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != lines[0]+"\n" {
		t.Fatalf("first line %q, %v — the stream was held back", first, err)
	}
	close(firstRead)
	rest, _ := io.ReadAll(reader)
	if string(rest) != lines[1]+"\n"+lines[2]+"\n" {
		t.Fatalf("the rest of the stream changed on the way: %q", rest)
	}
}

// While the orchestrator is away (draining before a restart), the API says
// so plainly instead of hanging or falling through to the console.
func TestServerSaysWhenTheOrchestratorIsAway(t *testing.T) {
	orchestrator := httptest.NewServer(http.NotFoundHandler())
	url := orchestrator.URL
	orchestrator.Close()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/plans", strings.NewReader("upload"))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	apiServer(url).routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "try again in a moment") {
		t.Fatalf("want a 503 that says to retry, got %d %s", rec.Code, rec.Body.String())
	}
}

// memberRuntime makes dev@example.com a developer of the team space, and
// nothing more.
type memberRuntime struct{ planRuntime }

func (memberRuntime) SpaceConfig(_ context.Context, space string) (config.SpaceConfig, error) {
	if space == "team" {
		return config.SpaceConfig{Developers: config.AccessConfig{Users: []string{"dev@example.com"}}}, nil
	}
	return config.SpaceConfig{}, nil
}

// The space a deploy lands in is only known once the upload is read, so it
// is authorized in the orchestrator, against the caller the server forwarded
// — through both hops, exactly as when one process did both.
func TestForwardedUploadsAuthorizeTheSpaceTheyName(t *testing.T) {
	rt := memberRuntime{planRuntime{platform: &config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"root@example.com"}}}}}
	orchestrator := planServer(rt, &ai.Analysis{AppType: "web"})
	orchestratorHTTP := httptest.NewServer(orchestrator.orchestratorRoutes())
	defer orchestratorHTTP.Close()
	api := apiServer(orchestratorHTTP.URL)
	api.rt = rt
	apiHTTP := httptest.NewServer(api.routes())
	defer apiHTTP.Close()
	developer := sealedToken(t, testSessionKey, "dev@example.com")

	send := func(path string, body []byte) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, apiHTTP.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+developer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}

	// The developer's own space: read and planned, through both processes.
	status, body := send("/v1/plans?space=team", appUpload(t, nil))
	var plan Plan
	if status != http.StatusOK || json.Unmarshal([]byte(body), &plan) != nil || plan.App.Space != "team" {
		t.Fatalf("plan in the developer's space: %d %s", status, body)
	}

	// A donkey.yaml naming somebody else's space is refused once it is read,
	// for a plan and for a deploy alike.
	elsewhere := appUpload(t, map[string]string{"donkey.yaml": "name: web\nport: 4000\nspace: secret\n"})
	for _, path := range []string{"/v1/plans", "/v1/deploys"} {
		status, body := send(path, elsewhere)
		if status != http.StatusForbidden || !strings.Contains(body, errSpaceForbidden.Error()) {
			t.Fatalf("%s into another space: want 403, got %d %s", path, status, body)
		}
	}
	// So is a query parameter pointing there, whatever donkey.yaml says.
	if status, body := send("/v1/deploys?space=secret&name=web&port=4000", appUpload(t, nil)); status != http.StatusForbidden {
		t.Fatalf("deploy with ?space=secret: want 403, got %d %s", status, body)
	}
}

// donkey doctor reaches BuildKit only through the orchestrator now; when the
// orchestrator is down, doctor says that rather than failing outright.
func TestDoctorAsksTheOrchestratorForBuildChecks(t *testing.T) {
	orchestrator := newFakeOrchestrator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/doctor" || r.Header.Get(orchestratorTokenHeader) != orchestratorToken(testSessionKey) {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"checks": []DoctorCheck{{Name: "Build engine (in-cluster BuildKit)", OK: true}}})
	})
	req := httptest.NewRequest(http.MethodGet, "https://donkey.example.com/v1/doctor", nil)
	asAdmin(t, req)
	checks := apiServer(orchestrator.URL).orchestratorChecks(req)
	if len(checks) != 2 || !checks[0].OK || checks[0].Name != "Orchestrator (donkey-orchestrator)" || checks[1].Name != "Build engine (in-cluster BuildKit)" {
		t.Fatalf("checks = %+v", checks)
	}
	if got := orchestrator.calls()[0].Get("X-Access-Token"); got != req.Header.Get("X-Access-Token") {
		t.Fatal("doctor must ask with the caller's own token")
	}

	away := httptest.NewServer(http.NotFoundHandler())
	away.Close()
	checks = apiServer(away.URL).orchestratorChecks(req)
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "not answering") {
		t.Fatalf("an absent orchestrator should be one failed check, got %+v", checks)
	}
}

// Every forwarded pattern exists in the orchestrator — they come from one
// list — and none of them answers on the API without the forward.
func TestOrchestratedRoutesAreServedByTheOrchestrator(t *testing.T) {
	s := drainingOrchestrator()
	for _, route := range s.orchestrated() {
		method, path, _ := strings.Cut(route.pattern, " ")
		path = strings.NewReplacer("{id}", "p1", "{space}", "team", "{app}", "web", "{service}", "db").Replace(path)
		req := httptest.NewRequest(method, "http://donkey-orchestrator"+path, nil)
		rec := httptest.NewRecorder()
		s.orchestratorRoutes().ServeHTTP(rec, req)
		// Unauthenticated, so refused — but by the orchestrator's own guard,
		// not a missing route.
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "donkey-server") {
			t.Errorf("%s: %d %s", route.pattern, rec.Code, rec.Body.String())
		}
	}
}
