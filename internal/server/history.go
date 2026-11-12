package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/deployment"
	"github.com/adrianliechti/donkey/internal/detect"
	"github.com/adrianliechti/donkey/internal/history"
	"github.com/adrianliechti/donkey/internal/identity"
)

// The source history: every deploy's upload as a git commit by the person
// who deployed, and what was actually built on top as a commit by the
// platform — the recipe the assistant wrote, its repairs, and the settings
// the build ran with (see internal/history for the layout). Recording is
// best-effort like the release record beside it: a deploy that already
// happened never fails over its history.

// commitListLimit bounds one history listing.
const commitListLimit = 200

// settingsFile is where a build's commit keeps the configuration it ran
// with, beside the recipe in the app's folder.
const settingsFile = ".donkey/deploy.yaml"

// sourceCapture is what a deploy leaves for its source history.
type sourceCapture struct {
	// root is the uploaded build context. The upload outlives the deploy
	// that read it, so it is still on disk when the release is persisted.
	root string
	// appPath is the app's folder inside root ("" when it is root).
	appPath string
	// revision is the git commit the CLI deployed from, when it knew one.
	revision string
	// changes is what the build changed in its own copy of root.
	changes []history.FileChange
}

// captureSource compares the build's copy with the upload. A comparison that
// fails still records the upload; only the platform's half is lost.
func captureSource(uploaded, workspace, appPath, revision string) *sourceCapture {
	capture := &sourceCapture{root: uploaded, revision: revision}
	if appPath = filepath.ToSlash(appPath); appPath != "." {
		capture.appPath = appPath
	}
	changes, err := buildChanges(uploaded, workspace)
	if err != nil {
		log.Printf("source history: comparing the build's copy of %s: %v", uploaded, err)
		return capture
	}
	capture.changes = changes
	return capture
}

// buildChanges lists the files that differ between the upload and the
// build's copy of it: written, changed, or removed by the build.
func buildChanges(uploaded, workspace string) ([]history.FileChange, error) {
	var changes []history.FileChange
	seen := map[string]bool{}
	err := filepath.WalkDir(workspace, func(full string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(workspace, full)
		if err != nil {
			return err
		}
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		same, err := sameFile(filepath.Join(uploaded, rel), full, info)
		if err != nil || same {
			return err
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		changes = append(changes, history.FileChange{
			Path: filepath.ToSlash(rel), Content: content, Executable: info.Mode()&0o111 != 0,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(uploaded, func(full string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(uploaded, full)
		if err != nil {
			return err
		}
		if !seen[rel] {
			changes = append(changes, history.FileChange{Path: filepath.ToSlash(rel), Deleted: true})
		}
		return nil
	})
	return changes, err
}

// sameFile compares an uploaded file with the build's copy: content and
// whether it is executable, which is all a commit records — the copy's exact
// bits follow the process umask, not the build.
func sameFile(original, copied string, info fs.FileInfo) (bool, error) {
	before, err := os.Stat(original)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if before.Size() != info.Size() || (before.Mode()&0o111 != 0) != (info.Mode()&0o111 != 0) {
		return false, nil
	}
	a, err := os.Open(original)
	if err != nil {
		return false, err
	}
	defer a.Close()
	b, err := os.Open(copied)
	if err != nil {
		return false, err
	}
	defer b.Close()
	bufA, bufB := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		n, errA := io.ReadFull(a, bufA)
		m, errB := io.ReadFull(b, bufB)
		if n != m || !bytes.Equal(bufA[:n], bufB[:m]) {
			return false, nil
		}
		if errA == io.EOF || errA == io.ErrUnexpectedEOF {
			return errB == io.EOF || errB == io.ErrUnexpectedEOF, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}

// recordSource commits a persisted deploy: the upload on main, the build's
// changes on platform, and tags the commit the build used with its number.
func (s *Server) recordSource(ctx context.Context, space, app string, release AppRelease, build int64) {
	capture := release.source
	if capture == nil {
		return
	}
	upload, err := s.history.RecordUpload(space, app, history.UploadInput{
		Dir:     capture.root,
		Author:  sourceAuthor(requestIdentity(ctx)),
		When:    releaseTime(release.StartedAt),
		Message: uploadMessage(app, capture),
	})
	if err != nil {
		log.Printf("source history app=%s space=%s: recording the upload: %v", app, space, err)
		return
	}
	built := upload
	changes := capture.changes
	if release.settings != nil {
		body, err := buildSettings(release.settings, build, release.Result.ImageRef, detect.Stack(release.Update.BaseImages))
		if err != nil {
			log.Printf("source history app=%s space=%s: rendering the build's settings: %v", app, space, err)
		} else {
			changes = append(slices.Clone(changes), history.FileChange{Path: path.Join(capture.appPath, settingsFile), Content: body})
		}
	}
	if len(changes) > 0 {
		platform, err := s.history.RecordPlatform(space, app, history.PlatformInput{
			Base:    upload,
			Changes: changes,
			When:    releaseTime(release.FinishedAt),
			Message: platformMessage(build, changes),
		})
		if err != nil {
			log.Printf("source history app=%s space=%s: recording the build's changes: %v", app, space, err)
		} else {
			built = platform
		}
	}
	if err := s.history.TagBuild(space, app, build, built); err != nil {
		log.Printf("source history app=%s space=%s: tagging build %d: %v", app, space, build, err)
	}
}

// buildSettings renders what a build ran with as donkey.yaml: the effective
// configuration, with the console's choices and the deploy's decisions in
// it, so every build in the source history says how it was deployed. Env
// values and service parameters stay out — the history is permanent and can
// be downloaded, and the donkey.yaml that was uploaded is in the commit
// beneath anyway.
func buildSettings(app *config.AppConfig, build int64, image, stack string) ([]byte, error) {
	out := *app
	out.Env = nil
	if out.Storage != nil && len(out.Storage.Paths) == 0 {
		out.Storage = nil
	}
	out.Addons = make([]config.AddonConfig, len(app.Addons))
	for i, addon := range app.Addons {
		addon.Parameters = nil
		out.Addons[i] = addon
	}
	body, err := out.Render()
	if err != nil {
		return nil, err
	}
	which := "this build"
	if build > 0 {
		which = fmt.Sprintf("build #%d", build)
	}
	header := "# What " + which + " ran with, written by donkey for the record and never\n# read back: donkey.yaml is the file you edit.\n"
	if image != "" {
		header += "# image: " + image + "\n"
	}
	if stack != "" {
		header += "# stack: " + stack + "\n"
	}
	return append([]byte(header), body...), nil
}

// sourceAuthor names the person behind a deploy the way space membership
// knows them: by name and email, or by subject when the IdP sent neither.
func sourceAuthor(id *identity.Identity) history.Author {
	if id == nil {
		return history.Author{Name: "unknown"}
	}
	name := id.Name
	if name == "" {
		name = id.PreferredUsername
	}
	if name == "" && id.Email == "" {
		name = "sub:" + id.Subject
	}
	return history.Author{Name: name, Email: id.Email}
}

func releaseTime(value string) time.Time {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Now()
}

func uploadMessage(app string, capture *sourceCapture) string {
	subject := "Deploy " + app
	if capture.revision != "" {
		subject += " at " + capture.revision
	}
	var body []string
	if capture.appPath != "" {
		body = append(body, "The app is in "+capture.appPath+"/ of this build context.")
	}
	return strings.Join(append([]string{subject}, body...), "\n\n")
}

// platformMessage says in a line what the build did to the upload: wrote
// its recipe, repaired source, or both — then each file on its own line. A
// build that did neither still records its settings, and says only that.
func platformMessage(build int64, changes []history.FileChange) string {
	var recipe, repaired int
	lines := make([]string, 0, len(changes))
	// Paths are relative to the build context, so in a monorepo the recipe
	// sits in the app's own folder: web/.donkey/Dockerfile.
	recipeDir := path.Dir(detect.AIDockerfilePath) + "/"
	for _, change := range changes {
		verb := "Repaired"
		switch {
		case change.Path == settingsFile || strings.HasSuffix(change.Path, "/"+settingsFile):
			verb = "Recorded"
		case strings.HasPrefix(change.Path, recipeDir) || strings.Contains(change.Path, "/"+recipeDir):
			recipe++
			verb = "Wrote"
		case change.Deleted:
			repaired++
			verb = "Removed"
		default:
			repaired++
		}
		lines = append(lines, "- "+verb+" "+change.Path)
	}
	var parts []string
	if recipe > 0 {
		parts = append(parts, "wrote the build recipe")
	}
	if repaired == 1 {
		parts = append(parts, "repaired 1 file")
	} else if repaired > 1 {
		parts = append(parts, fmt.Sprintf("repaired %d files", repaired))
	}
	if len(parts) == 0 {
		parts = append(parts, "recorded its settings")
	}
	subject := "The build " + strings.Join(parts, " and ")
	if build > 0 {
		subject = fmt.Sprintf("Build #%d %s", build, strings.Join(parts, " and "))
	}
	return subject + "\n\n" + strings.Join(lines, "\n")
}

// SourceBuild is one build that used a commit. OK is unknown (nil) once the
// build's record has aged out of the bounded build history.
type SourceBuild struct {
	Sequence int64 `json:"sequence"`
	OK       *bool `json:"ok,omitempty"`
}

// SourceCommit is a history entry with its builds resolved, and whether it
// is what the app runs right now.
type SourceCommit struct {
	history.Commit
	Builds []SourceBuild `json:"builds,omitempty"`
	Live   bool          `json:"live,omitempty"`
}

type SourceCommitDetail struct {
	SourceCommit
	Changes []history.Change `json:"changes"`
}

// buildIndex is what the build history knows about an app's builds, and the
// image the app runs. Both are best-effort: without them a commit still
// shows, just without outcomes.
type buildIndex struct {
	builds  map[int64]deployment.BuildSummary
	running string
}

func (s *Server) buildIndex(ctx context.Context, space, app string) buildIndex {
	index := buildIndex{builds: map[int64]deployment.BuildSummary{}}
	builds, err := s.deploymentStore().Builds(ctx, space, app)
	if err != nil {
		return index
	}
	for _, b := range builds {
		index.builds[b.Sequence] = b
	}
	if status, err := s.rt.Status(ctx, space, app); err == nil && status != nil {
		index.running = status.Image
	}
	return index
}

// commit resolves a commit's builds. It is live when one of them released
// the running image — any of them: a no-build redeploy (an addon change, a
// rollback) adds a build record of the same image without a commit.
func (index buildIndex) commit(c history.Commit) SourceCommit {
	out := SourceCommit{Commit: c}
	for _, sequence := range c.Builds {
		build := SourceBuild{Sequence: sequence}
		if summary, ok := index.builds[sequence]; ok {
			build.OK = &summary.OK
			if summary.OK && index.running != "" && summary.Image == index.running {
				out.Live = true
			}
		}
		out.Builds = append(out.Builds, build)
	}
	return out
}

// handleCommits: GET /v1/spaces/{space}/apps/{app}/commits — the history,
// newest first.
func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	commits, err := s.history.Log(space, app, commitListLimit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	index := s.buildIndex(r.Context(), space, app)
	out := make([]SourceCommit, 0, len(commits))
	for _, c := range commits {
		out = append(out, index.commit(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": out})
}

// handleCommit: GET .../commits/{commit} — one commit and the files it changed.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	space, app, ok := s.historyRequest(w, r)
	if !ok {
		return
	}
	detail, err := s.history.Show(space, app, r.PathValue("commit"))
	if err != nil {
		writeHistoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SourceCommitDetail{
		SourceCommit: s.buildIndex(r.Context(), space, app).commit(detail.Commit),
		Changes:      detail.Changes,
	})
}

// handleCommitDiff: GET .../commits/{commit}/diff?path= — one file's change.
func (s *Server) handleCommitDiff(w http.ResponseWriter, r *http.Request) {
	space, app, ok := s.historyRequest(w, r)
	if !ok {
		return
	}
	diff, err := s.history.Diff(space, app, r.PathValue("commit"), r.URL.Query().Get("path"))
	if err != nil {
		writeHistoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleCommitTree: GET .../commits/{commit}/tree — every file of a version.
func (s *Server) handleCommitTree(w http.ResponseWriter, r *http.Request) {
	space, app, ok := s.historyRequest(w, r)
	if !ok {
		return
	}
	files, err := s.history.Tree(space, app, r.PathValue("commit"))
	if err != nil {
		writeHistoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleCommitFile: GET .../commits/{commit}/file?path=[&download=true].
// Like the disk browser, an app's file is never served as something the
// browser would run on the console's origin: plain text to show it, an
// attachment to keep it.
func (s *Server) handleCommitFile(w http.ResponseWriter, r *http.Request) {
	space, app, ok := s.historyRequest(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	content, err := s.history.ReadFile(space, app, r.PathValue("commit"), p)
	if err != nil {
		writeHistoryErr(w, err)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	// The console shows text and offers binary files for download instead.
	if history.IsBinary(content) {
		w.Header().Set("X-Donkey-Binary", "true")
	}
	out := &downloadWriter{w: w, name: path.Base(p), inline: r.URL.Query().Get("download") != "true"}
	_, _ = out.Write(content)
	out.finish()
}

// handleCommitArchive: GET .../commits/{commit}/archive — the version as a
// zip, its files inside one folder named after the app and commit, the way
// a source download from any git host looks. The zip is complete before a
// byte is sent: the repository is locked only as long as reading it takes,
// never as long as a slow download, and a failure is still an error rather
// than a truncated file.
func (s *Server) handleCommitArchive(w http.ResponseWriter, r *http.Request) {
	space, app, ok := s.historyRequest(w, r)
	if !ok {
		return
	}
	commit := r.PathValue("commit")
	name := app + "-" + commit[:min(len(commit), 12)]
	zip, err := os.CreateTemp("", "donkey-archive-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.Remove(zip.Name())
	defer zip.Close()
	buffered := bufio.NewWriterSize(zip, 64<<10)
	err = s.history.Archive(space, app, commit, name+"/", buffered)
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		writeHistoryErr(w, err)
		return
	}
	size, err := zip.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = zip.Seek(0, io.SeekStart)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	out := &downloadWriter{w: w, name: name + ".zip"}
	if _, err := io.Copy(out, zip); err != nil {
		log.Printf("source history: archive of %s %s stopped early: %v", app, commit, err)
		return
	}
	out.finish()
}

func (s *Server) historyRequest(w http.ResponseWriter, r *http.Request) (space, app string, ok bool) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return "", "", false
	}
	return space, app, true
}

func writeHistoryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, history.ErrNotFound):
		writeErr(w, http.StatusNotFound, errors.New("that version or file is not in this app's history"))
	case errors.Is(err, history.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}
