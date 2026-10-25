package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/donkey/internal/config"
)

// The deploy board is what donkey-orchestrator is working on right now, and
// what it finished a short while ago. A deploy runs on here whoever started
// it and whether or not they are still watching — the CLI's terminal or a
// closed browser tab — and an app that is being deployed for the first time
// does not exist anywhere else yet. The board is how the console shows it
// anyway: a placeholder with its stage, and the outcome once it is over.
//
// It is process memory, like the plans and the per-app locks: the
// orchestrator is a single replica, and a restart drains the deploys under
// way before it goes.

// deployBoardRetention is how long a finished deploy stays on the board: long
// enough for whoever started it to come back and see how it went.
const deployBoardRetention = 15 * time.Minute

// Deploy stages beyond the ones the pipeline reports (detecting, building,
// releasing).
const (
	stageQueued = "queued"
	stageDone   = "done"
	stageFailed = "failed"
)

// ActiveDeploy is one deploy on the board.
type ActiveDeploy struct {
	Space string `json:"space"`
	App   string `json:"app"`
	// Stage is queued, detecting, building, releasing — and done or failed
	// once it is over.
	Stage string `json:"stage"`
	// Line is the latest progress line, as the deploy's own log said it.
	Line string `json:"line,omitempty"`
	// By is who started it; Via is console or cli.
	By         string     `json:"by,omitempty"`
	Via        string     `json:"via"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type deployBoard struct {
	mu      sync.Mutex
	next    uint64
	entries map[string]*boardEntry
}

type boardEntry struct {
	id uint64
	ActiveDeploy
}

// deployTicket is one deploy's handle on its board entry. A newer deploy of
// the same app takes the entry over; the older ticket then writes nothing.
// A nil ticket (a dry run) is a no-op.
type deployTicket struct {
	board *deployBoard
	key   string
	id    uint64
}

func (b *deployBoard) active() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, entry := range b.entries {
		if entry.FinishedAt == nil {
			return true
		}
	}
	return false
}

func (b *deployBoard) start(space, app, by, via string, now time.Time) *deployTicket {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]*boardEntry{}
	}
	b.next++
	key := space + "/" + app
	b.entries[key] = &boardEntry{id: b.next, ActiveDeploy: ActiveDeploy{
		Space: space, App: app, Stage: stageQueued, By: by, Via: via, StartedAt: now.UTC(),
	}}
	return &deployTicket{board: b, key: key, id: b.next}
}

func (t *deployTicket) update(change func(*ActiveDeploy)) {
	if t == nil {
		return
	}
	t.board.mu.Lock()
	defer t.board.mu.Unlock()
	if entry := t.board.entries[t.key]; entry != nil && entry.id == t.id && entry.FinishedAt == nil {
		change(&entry.ActiveDeploy)
	}
}

func (t *deployTicket) stage(stage string) {
	t.update(func(d *ActiveDeploy) { d.Stage = stage })
}

func (t *deployTicket) line(line string) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "·"))
	if line == "" {
		return
	}
	if len(line) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	t.update(func(d *ActiveDeploy) { d.Line = line })
}

// finish records the outcome once; later calls change nothing, so a handler
// can defer a fallback for the paths that return early.
func (t *deployTicket) finish(ok bool, failure string) {
	t.update(func(d *ActiveDeploy) {
		now := time.Now().UTC()
		d.FinishedAt = &now
		if ok {
			d.Stage, d.Error = stageDone, ""
		} else {
			d.Stage, d.Error = stageFailed, failure
		}
	})
}

// list returns what the caller may see, oldest first, after dropping what
// finished longer ago than the retention.
func (b *deployBoard) list(now time.Time, visible func(space string) bool) []ActiveDeploy {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []ActiveDeploy{}
	for key, entry := range b.entries {
		if entry.FinishedAt != nil && now.Sub(*entry.FinishedAt) > deployBoardRetention {
			delete(b.entries, key)
			continue
		}
		if visible(entry.Space) {
			out = append(out, entry.ActiveDeploy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// deployer names who started a deploy, the way the source history does.
func deployer(r *http.Request) string {
	id := requestIdentity(r.Context())
	if id == nil {
		return ""
	}
	switch {
	case id.Name != "":
		return id.Name
	case id.Email != "":
		return id.Email
	default:
		return id.PreferredUsername
	}
}

// handleDeploys: GET /v1/deploys — the board, limited to the spaces the
// caller can see.
func (s *Server) handleDeploys(w http.ResponseWriter, r *http.Request) {
	settings, err := s.listSpaceConfigs(r.Context())
	if err != nil {
		writeAccessError(w, err)
		return
	}
	roles := s.spaceList(r.Context(), settings).Roles
	deploys := s.board.list(time.Now(), func(space string) bool { return roles[space] != "" })
	for i := range deploys {
		if roles[deploys[i].Space] == config.SpaceViewer {
			// Progress and failures can contain build output and application logs.
			deploys[i].Line, deploys[i].Error = "", ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deploys": deploys,
	})
}
