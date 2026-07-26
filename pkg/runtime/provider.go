// Package runtime abstracts where donkey runs apps. A Provider owns one
// runtime's whole vocabulary — Kubernetes speaks namespaces, Deployments and
// server-side apply; Docker speaks networks, containers and volumes — and
// exposes only donkey's own verbs: deploy an app, wire a service, read the
// logs, browse the disk. The control plane (internal/server) orchestrates the
// runtime-neutral flow (build, analysis, addon contracts, streaming) and
// never sees the vocabulary underneath.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// Provider is one runtime. Every method takes donkey's names (space, app,
// service) — never the runtime's own (namespace, Deployment, container).
//
// Methods that change shared state (Deploy, ApplyNetwork, EnsureService) are
// serialized per app by the caller; providers don't need their own locks.
type Provider interface {
	// --- Spaces -----------------------------------------------------------

	// ValidateSpace says whether the name may be a space at all: donkey's
	// naming rules, plus whatever names this runtime reserves for itself.
	// Pure — no I/O, callable on every request.
	ValidateSpace(space string) error
	// CheckSpaceFree refuses a space whose place in the runtime exists but
	// isn't donkey's — a space name must never take over (or later delete)
	// somebody else's namespace or network.
	CheckSpaceFree(ctx context.Context, space string) error
	ListSpaces(ctx context.Context) ([]string, error)
	CreateSpace(ctx context.Context, space string) error
	// DeleteSpace removes an app-less space with its services and their data;
	// it refuses a space that still holds apps.
	DeleteSpace(ctx context.Context, space string) error

	// --- Apps -------------------------------------------------------------

	ListApps(ctx context.Context) ([]AppRef, error)
	Status(ctx context.Context, space, app string) (*AppStatus, error)
	// HasData reports whether the app owns a persistent disk — true even
	// while the app is stopped or crash-looping, which is exactly when
	// someone needs to look at what is on it.
	HasData(ctx context.Context, space, app string) bool
	// Restart rolls the app without changing anything else. False (and no
	// error) when nothing with this name is deployed.
	Restart(ctx context.Context, space, app string) (bool, error)
	Logs(ctx context.Context, space, app string, opts LogOptions, out io.Writer) error
	// Evidence collects what a human (or the AI assistant) needs to see why
	// an app is unhealthy right now.
	Evidence(ctx context.Context, space, app string) Evidence

	// Deploy takes a built image live: everything after the build, in this
	// runtime's own choreography — storage, declared services, the app
	// itself, its addresses — narrated through progress. Failures carry
	// their evidence: *JobError for a failed pre-deploy step, *RolloutError
	// for an app that never became healthy.
	Deploy(ctx context.Context, spec DeploySpec, progress Progress) error
	// Render returns what Deploy would create, as text for human review,
	// without touching the runtime. Credentials are left out — the result
	// lands in terminals and CI logs.
	Render(spec DeploySpec) (string, error)
	// Destroy removes the app and everything donkey created for it; keepData
	// spares the disk (and the data of services only this app used).
	Destroy(ctx context.Context, space, app string, keepData bool) error

	// --- Env --------------------------------------------------------------

	GetEnv(ctx context.Context, space, app string) (map[string]string, error)
	SetEnv(ctx context.Context, space, app string, env map[string]string) error

	// --- App metadata -----------------------------------------------------

	// AppMeta and SetAppMeta keep small records on a deployed app (the
	// console's network choices, the last deploy's decisions) that must
	// survive deploys. A missing app or key reads as ""; writing nil
	// removes the key.
	AppMeta(ctx context.Context, space, app, key string) (string, error)
	SetAppMeta(ctx context.Context, space, app, key string, value *string) error

	// --- Network ----------------------------------------------------------

	// Network reads how the app is reached from the runtime's actual state,
	// never from stored intent. (Whether the app exists at all is Status's
	// question — an undeployed app simply has no routes.)
	Network(ctx context.Context, space, app string) (*NetworkState, error)
	// ApplyNetwork changes how the app is reached — without a deploy. The
	// config carries the choice (Public/Internal/Domain/Access) the same way
	// a donkey.yaml would; existing shared-address routes keep serving and
	// follow the app's exposure. The note, when not empty, is worth showing
	// (e.g. "the certificate is still being issued").
	ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (note string, err error)
	// SetAccess narrows (or reopens) who may open a login-protected app.
	// Takes effect within seconds; no deploy, no restart.
	SetAccess(ctx context.Context, space, app string, access *config.AccessConfig) error

	// --- Services (addons) ------------------------------------------------

	// ServiceBinding returns the env contract of a service instance: the
	// space's own for owner == addons.SharedOwner, an app's attached copy
	// for owner == the app. Empty map when there is none.
	ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error)
	// EnsureService creates the space's service if it is missing (waiting
	// until it serves) and re-applies it if it exists; either way it returns
	// the binding. Version and storage echo what the instance actually uses.
	EnsureService(ctx context.Context, space string, spec ServiceSpec) (binding map[string]string, version, storage string, err error)
	// AttachService gives the app its copy of the service's binding and
	// wires it into the app's env. Restarted reports whether a running app
	// picked it up on the spot (an undeployed one gets it at first deploy).
	AttachService(ctx context.Context, space, app, name string, binding map[string]string) (restarted bool, err error)
	// DetachService unwires the binding from the app's env and removes the
	// app's copy. The service and its data stay.
	DetachService(ctx context.Context, space, app, name string) error
	// RemoveService removes a space's service, detaching every app that uses
	// it first; returns who was detached.
	RemoveService(ctx context.Context, space, name string, keepData bool) (detached []string, err error)
	// ServiceAttachments names the apps holding a copy of the service's
	// binding.
	ServiceAttachments(ctx context.Context, space, name string) ([]string, error)
	// ServiceEndpoint is the service's in-runtime address, for tunnels. An
	// empty host means the service runs elsewhere (external binding).
	ServiceEndpoint(ctx context.Context, space, name string) (host string, port int32, err error)
	// ListServices lists the bindings an owner holds: the space's services
	// for addons.SharedOwner, an app's attachments for the app.
	ListServices(ctx context.Context, space, owner string) ([]ServiceBindingInfo, error)
	// SharedServiceValues collects one binding key's values across every
	// space's services — the AI proxy's token list.
	SharedServiceValues(ctx context.Context, key string) ([]string, error)

	// --- Files ------------------------------------------------------------

	// The app's disk as a file browser. Paths are relative to the disk's
	// root, where each storage path of the app has its own folder.
	ListFiles(ctx context.Context, space, app, path string) ([]FileEntry, error)
	StorageFolders(ctx context.Context, space, app string) ([]StorageFolder, error)
	ReadFile(ctx context.Context, space, app, path string, out io.Writer) error
	WriteFile(ctx context.Context, space, app, path string, in io.Reader) error
	RemoveFile(ctx context.Context, space, app, path string) error
	MoveFile(ctx context.Context, space, app, from, to string) error
	MakeDir(ctx context.Context, space, app, path string) error

	// --- Health -----------------------------------------------------------

	// Checks probes this runtime's own dependencies for doctor — the pieces
	// only the provider knows it needs.
	Checks(ctx context.Context) []Check
}

// AppRef identifies one deployed app.
type AppRef struct {
	Space string
	App   string
}

// AppStatus is a plain snapshot of an app's health for `donkey status`.
type AppStatus struct {
	Found        bool
	Image        string
	ReadyReplica int32
	WantReplica  int32
	// UpToDate reports that the observed state belongs to the latest
	// generation — without it, an old healthy instance would make a failing
	// rollout look green.
	UpToDate bool
	// Instances are the app's running units (pods, containers), by name.
	Instances []string
	// IngressHost is the hostname the app serves on; empty for internal.
	IngressHost string
	// Protected says traffic reaches the app through the login gateway.
	Protected bool
	// Port the app listens on (donkey.yaml's `port`).
	Port int32
	// Restarts sums restarts across the app's instances — the number that
	// separates "quietly struggling" from "running".
	Restarts int32
	// StartedAt is when the newest instance came up ("deployed X ago").
	StartedAt time.Time
}

// Healthy reports whether the app's latest version is fully up.
func (s *AppStatus) Healthy() bool {
	return s.Found && s.UpToDate && s.ReadyReplica >= s.WantReplica
}

func (s *AppStatus) ReadyText() string {
	return fmt.Sprintf("%d of %d running", s.ReadyReplica, s.WantReplica)
}

type LogOptions struct {
	// Component selects which part of the app: "web" or "addon-<name>".
	Component string
	Follow    bool
	TailLines int64
	Since     time.Duration
}

// DeploySpec is everything Deploy and Render need: the settled app config
// (space set, analysis merged) and the image to run.
type DeploySpec struct {
	App      *config.AppConfig
	Space    string
	ImageRef string
	// Decisions is the JSON record of what this deploy settled (storage,
	// health route), persisted with the app so a no-build deploy keeps the
	// same shape. Empty means nothing to record.
	Decisions string
	// NoWait skips waiting for the app to become healthy.
	NoWait bool
}

// Progress narrates long operations back to the user, line by line.
type Progress interface {
	Line(format string, a ...any)
}

// NopProgress swallows progress lines, for callers with nowhere to put them.
type NopProgress struct{}

func (NopProgress) Line(string, ...any) {}

// Evidence is what donkey collects when an app is (or went) unhealthy:
// enough for a human to see the problem and for the AI assistant to
// diagnose it.
type Evidence struct {
	// Summaries describe the app's instances, one line each.
	Summaries []string
	// Events are the runtime's warnings about them.
	Events []string
	// Logs are the last lines from the most tellingly broken instance.
	Logs string
}

// RolloutError means the new version never became healthy.
type RolloutError struct {
	App      string
	Evidence Evidence
	Reason   string
}

func (e *RolloutError) Error() string {
	return fmt.Sprintf("the new version of %s didn't become healthy: %s", e.App, e.Reason)
}

// JobError means a run-to-completion step (pre-deploy) failed.
type JobError struct {
	Reason string
	Logs   string
}

func (e *JobError) Error() string { return e.Reason }

// UserError marks a failure the caller caused (a bad type, name or URL), so
// API handlers answer 400 instead of blaming the runtime.
type UserError struct{ Err error }

func (e UserError) Error() string { return e.Err.Error() }
func (e UserError) Unwrap() error { return e.Err }

// PathRouteState is one shared-address route as it exists in the runtime.
type PathRouteState struct {
	Name string
	Host string
	Path string
	// Protected says the route sends its traffic through the login gateway.
	Protected bool
}

// NetworkState is what the runtime currently says about how an app is
// reached — read from actual state, so the console shows what is true
// rather than what was meant.
type NetworkState struct {
	// RouteFound: the app has an address of its own (not internal).
	RouteFound bool
	Host       string
	Protected  bool
	// Access are the rules the gateway enforces on the app's own route.
	Access *config.AccessConfig
	Routes []PathRouteState
}

// ServiceSpec asks for a service instance of the space.
type ServiceSpec struct {
	Type    string
	Name    string
	Version string
	Storage string
	// External marks a service running elsewhere: nothing is provisioned,
	// the binding is derived from URL.
	External bool
	URL      string
}

// ServiceBindingInfo is one binding as the console may see it: the name and
// the injected env var names — never the values.
type ServiceBindingInfo struct {
	Name string
	Keys []string
}

// StorageFolder maps a folder on the app's disk to the place the app sees
// it, so a listing of the disk's root can say what "app-data" actually is.
type StorageFolder struct {
	// Name is the folder on the disk.
	Name string
	// Path is where the app has it mounted, e.g. /app/data.
	Path string
}

// FileEntry is one item on the app's disk.
type FileEntry struct {
	Name string
	Size int64
	// Mode is the ls-style spelling, e.g. "-rw-rw-r--".
	Mode    string
	ModTime time.Time
	IsDir   bool
	// IsLink marks a shortcut: it is neither a file to download nor a folder
	// to open, and callers must not offer either.
	IsLink bool
}

// Check is one doctor probe of a runtime dependency.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// App metadata keys. They live with the deployed app (annotations, labels —
// the provider's choice) and survive deploys.
const (
	// MetaConsole records the console's network choices, written by the
	// console endpoints only — a deploy must never wipe them.
	MetaConsole = "donkey.dev/console"
	// MetaDecisions records what deploy-time analysis settled (storage,
	// health route), so a no-build deploy keeps the same shape.
	MetaDecisions = "donkey.dev/decisions"
)

// File-browser limits, shared by every provider.
var (
	// MaxUploadBytes caps a single upload — a browser tab must not be able
	// to fill the app's disk in one request.
	MaxUploadBytes int64 = 200 << 20
	// MaxListEntries caps one folder listing. A folder with a million files
	// would otherwise turn one click into a huge response.
	MaxListEntries = 2000
)

// File-browser errors, shared by every provider so handlers can map them to
// honest status codes.
var (
	// ErrNoDisk means the app has no persistent disk at all — the common
	// case, and not a failure.
	ErrNoDisk = errors.New("this app has no disk — add storage.paths to donkey.yaml and deploy again to give it one")
	// ErrNotDeployed means the app isn't running in this space at all.
	ErrNotDeployed = errors.New("nothing with this name is deployed — deploy the app first")
	// ErrBadPath rejects a path before it ever reaches the runtime.
	ErrBadPath = errors.New("that path isn't allowed")
	// ErrNotFound is "nothing at that path on the disk".
	ErrNotFound = errors.New("there is nothing at that path on the disk")
	// ErrNotAFile / ErrNotAFolder separate the two obvious mix-ups.
	ErrNotAFile   = errors.New("that is a folder, not a file")
	ErrNotAFolder = errors.New("that is a file, not a folder")
	// ErrTooLarge is the upload cap.
	ErrTooLarge = errors.New("that file is too large to upload")
)
