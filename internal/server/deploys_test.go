package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestDeployBoardHidesLogsFromViewers(t *testing.T) {
	s, rt := spaceTestServer()
	policy := rt.settings["team"]
	policy.Viewers = config.AccessConfig{Users: []string{"viewer@example.com"}}
	policy.Developers.Users = []string{"developer@example.com"}
	policy.Developers.Groups = nil
	rt.settings["team"] = policy
	ticket := s.board.start("team", "web", "owner@example.com", "cli", time.Now())
	ticket.line("Last lines from the app: DATABASE_URL=postgres://user:secret@db/private")
	ticket.finish(false, "rollout failed: postgres://user:secret@db/private")
	s.board.start("other", "hidden", "other@example.com", "cli", time.Now())
	for _, user := range []string{"viewer@example.com", "developer@example.com", "owner@example.com"} {
		rec := callAs(t, s, user, "GET", "/v1/deploys", nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", user, rec.Code, rec.Body)
		}
		var response struct {
			Deploys []ActiveDeploy `json:"deploys"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Deploys) != 1 || response.Deploys[0].Stage != stageFailed || response.Deploys[0].FinishedAt == nil {
			t.Fatalf("lost status or exposed another space: %s", rec.Body)
		}
		got := response.Deploys[0]
		if user == "viewer@example.com" {
			if got.Line != "" || got.Error != "" || strings.Contains(rec.Body.String(), "secret") {
				t.Fatalf("viewer received logs: %s", rec.Body)
			}
		} else if got.Line == "" || got.Error == "" {
			t.Fatalf("developer lost diagnostics: %s", rec.Body)
		}
	}
}

// A deploy is on the board from the moment it is accepted: queued, then the
// pipeline's stages, then its outcome — which stays for a while, and only
// for those who can see the space.
func TestDeployBoardFollowsADeploy(t *testing.T) {
	var board deployBoard
	now := time.Now()
	ticket := board.start("acme", "shop", "Ada", "cli", now)
	everyone := func(string) bool { return true }

	if got := board.list(now, everyone); len(got) != 1 || got[0].Stage != stageQueued || got[0].Via != "cli" || got[0].By != "Ada" {
		t.Fatalf("a new deploy is queued: %+v", got)
	}
	ticket.stage("building")
	ticket.line("· Image ready: registry.example.com/shop:1")
	if got := board.list(now, everyone)[0]; got.Stage != "building" || got.Line != "Image ready: registry.example.com/shop:1" {
		t.Fatalf("stage and line follow the pipeline: %+v", got)
	}
	ticket.finish(false, "the build failed")
	ticket.finish(true, "") // the deferred fallback must not overwrite the outcome
	got := board.list(now, everyone)[0]
	if got.Stage != stageFailed || got.Error != "the build failed" || got.FinishedAt == nil {
		t.Fatalf("the outcome stays: %+v", got)
	}
	ticket.stage("releasing")
	if board.list(now, everyone)[0].Stage != stageFailed {
		t.Fatal("a finished deploy takes no more updates")
	}

	if got := board.list(now, func(space string) bool { return space != "acme" }); len(got) != 0 {
		t.Fatalf("a space the caller cannot see stays hidden: %+v", got)
	}
	if got := board.list(now.Add(deployBoardRetention+time.Minute), everyone); len(got) != 0 {
		t.Fatalf("finished deploys leave the board after the retention: %+v", got)
	}
}

// A newer deploy of the same app takes its entry over; the older one, still
// unwinding, writes nothing into it.
func TestDeployBoardNewerDeployWins(t *testing.T) {
	var board deployBoard
	now := time.Now()
	older := board.start("acme", "shop", "Ada", "cli", now)
	newer := board.start("acme", "shop", "Grace", "console", now.Add(time.Second))
	older.finish(false, "replaced")
	newer.stage("detecting")
	got := board.list(now, func(string) bool { return true })
	if len(got) != 1 || got[0].By != "Grace" || got[0].Stage != "detecting" || got[0].FinishedAt != nil {
		t.Fatalf("the newer deploy owns the entry: %+v", got)
	}
	var dry *deployTicket
	dry.stage("building") // a dry run has no ticket; using it is harmless
	dry.finish(true, "")
}
