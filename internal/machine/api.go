package machine

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// API is the control plane as the machine itself calls it: donkey-server,
// with the sign-in of the machine's owner — the same one the CLI in the
// machine uses. Whatever the machine asks for here, it gets or is refused
// exactly as its owner would be.
type API struct {
	// Server is the API's address inside the runtime (DONKEY_SERVER_URL).
	Server string
	// Token is the owner's sealed sign-in (DONKEY_TOKEN).
	Token  string
	Client *http.Client
	Log    io.Writer
}

const (
	// maxArchiveSize and maxSourceSize bound what one fetch writes: the
	// download and what it unpacks to. The history keeps far less per version
	// (history.MaxSnapshotSize); these only keep a wrong answer from filling
	// the disk.
	maxArchiveSize = 512 << 20
	maxSourceSize  = 1 << 30
)

var sourceCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (a API) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a API) request(ctx context.Context, method, path string) (*http.Response, error) {
	if a.Server == "" || a.Token == "" {
		return nil, refuse(http.StatusConflict, "this machine has no donkey sign-in — restart it to give it one")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.Server, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Accept", "application/json")
	res, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("donkey isn't answering this machine: %w", err)
	}
	if res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer)
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		// Never relayed as 401: to the console that would read as its own
		// sign-in having ended.
		return nil, refuse(http.StatusConflict, "the donkey sign-in inside this machine has ended — restart the machine, then try again")
	case answer.Error != "":
		return nil, refuse(relayStatus(res.StatusCode), "%s", answer.Error)
	}
	return nil, refuse(relayStatus(res.StatusCode), "donkey answered %s", res.Status)
}

// relayStatus keeps what the owner did wrong (4xx) apart from what went
// wrong on the way.
func relayStatus(status int) int {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return status
	}
	return http.StatusBadGateway
}

// FetchSource writes one version of an app's source into dir and makes it
// the first commit of a repository there.
func (a API) FetchSource(ctx context.Context, source Source, dir string) error {
	// Only the shape is checked here; whether there is such an app, and
	// whether the owner may read it, is the API's to say.
	if !config.ValidName(source.Space) || !config.ValidName(source.App) {
		return refuse(http.StatusBadRequest, "%s/%s is not an app", source.Space, source.App)
	}
	if !sourceCommitRE.MatchString(source.Commit) {
		return refuse(http.StatusBadRequest, "%q is not a version of an app's source", source.Commit)
	}
	res, err := a.request(ctx, http.MethodGet, fmt.Sprintf("/v1/spaces/%s/apps/%s/commits/%s/archive",
		url.PathEscape(source.Space), url.PathEscape(source.App), source.Commit))
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// A zip is read from its end, so it lands in a file first — outside the
	// workspace, where nothing is listed or kept.
	archive, err := os.CreateTemp("", "donkey-source-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	size, err := io.Copy(archive, io.LimitReader(res.Body, maxArchiveSize+1))
	if err != nil {
		return fmt.Errorf("fetching the source of %s stopped early: %w", source.App, err)
	}
	if size > maxArchiveSize {
		return refuse(http.StatusBadGateway, "the source of %s is larger than a machine fetches (%d MB)", source.App, maxArchiveSize>>20)
	}
	if err := unpack(archive, size, dir); err != nil {
		return fmt.Errorf("unpacking the source of %s: %w", source.App, err)
	}
	a.baseline(ctx, dir, source)
	return nil
}

// unpack writes an archive of the source history into dir. Every file of it
// sits in one folder named after the app and the version; the folder here
// has a name of its own, so that level is dropped.
func unpack(archive io.ReaderAt, size int64, dir string) error {
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return err
	}
	var written int64
	for _, file := range reader.File {
		_, name, ok := strings.Cut(file.Name, "/")
		if !ok || name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		// The history holds plain files. Anything else — a link, a device —
		// is not something it wrote, and a name that leaves the folder is
		// refused rather than repaired.
		if !file.Mode().IsRegular() {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("%q would land outside the folder", file.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if file.Mode()&0o111 != 0 {
			mode = 0o755
		}
		n, err := unpackFile(file, target, mode, maxSourceSize-written)
		if err != nil {
			return err
		}
		if written += n; written > maxSourceSize {
			return errors.New("it is larger than a machine unpacks")
		}
	}
	return nil
}

func unpackFile(file *zip.File, target string, mode os.FileMode, room int64) (int64, error) {
	in, err := file.Open()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, room+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// baseline makes the fetched version the first commit of a repository in the
// folder. From there on, everything the owner and the agent change reads as
// a change — in the agent's own view of the repository, and in git diff. It
// is a convenience: a folder without it is still the app's source.
func (a API) baseline(ctx context.Context, dir string, source Source) {
	message := fmt.Sprintf("%s as deployed to %s\n\nVersion %s of the app's source history in donkey.",
		source.App, source.Space, source.Commit[:12])
	identity := os.Environ()
	// Commits here are the owner's (see gitIdentity). Where the machine was
	// not told who that is, this one commit is donkey's rather than missing.
	if !gitConfigured("user.name") || !gitConfigured("user.email") {
		identity = append(identity,
			"GIT_AUTHOR_NAME=donkey", "GIT_AUTHOR_EMAIL=donkey@localhost",
			"GIT_COMMITTER_NAME=donkey", "GIT_COMMITTER_EMAIL=donkey@localhost")
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "--all"},
		{"commit", "--quiet", "--message", message},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, identity
		if out, err := cmd.CombinedOutput(); err != nil {
			if a.Log != nil {
				fmt.Fprintf(a.Log, "donkey machine: %s has no first commit (git %s: %v) %s\n",
					filepath.Base(dir), args[0], err, strings.TrimSpace(string(out)))
			}
			return
		}
	}
}

func gitConfigured(key string) bool {
	out, err := exec.Command("git", "config", "--global", "--get", key).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// stopTimeout bounds one request to be stopped. The control plane answers
// once the runtime has taken the machine apart, which ends this process
// first more often than not.
const stopTimeout = 2 * time.Minute

// StopMachine asks the control plane to stop this machine — the same request
// its owner's Stop in the console makes, with the same sign-in.
func (a API) StopMachine(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	res, err := a.request(ctx, http.MethodPost, "/v1/machine/stop")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	return res.Body.Close()
}
