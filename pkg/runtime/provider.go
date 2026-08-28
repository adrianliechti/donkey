// Package runtime abstracts where donkey runs apps. A Provider owns one
// runtime's vocabulary — Kubernetes namespaces and Deployments, Docker
// networks and containers — and exposes only donkey's verbs. The control
// plane orchestrates the runtime-neutral flow and never sees underneath.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// Provider is one runtime. Every method takes donkey's names, never the
// runtime's own. Methods that change shared state are serialized per app by
// the caller, so providers need no locks of their own.
type Provider interface {
	// --- Spaces -----------------------------------------------------------

	// ValidateSpace is pure — no I/O, callable on every request.
	ValidateSpace(space string) error
	// CheckSpaceFree refuses a space whose place in the runtime exists but
	// isn't donkey's, so a name can never take over (or later delete) someone
	// else's namespace or network.
	CheckSpaceFree(ctx context.Context, space string) error
	ListSpaces(ctx context.Context) ([]string, error)
	CreateSpace(ctx context.Context, space string) error
	// DeleteSpace removes an app-less space with its services and their data;
	// it refuses one that still holds apps.
	DeleteSpace(ctx context.Context, space string) error

	// --- Apps -------------------------------------------------------------

	ListApps(ctx context.Context) ([]AppRef, error)
	Status(ctx context.Context, space, app string) (*AppStatus, error)
	// HasData is true even while the app is stopped or crash-looping — which
	// is exactly when someone needs to look at the disk.
	HasData(ctx context.Context, space, app string) bool
	// Restart returns false with no error when nothing is deployed.
	Restart(ctx context.Context, space, app string) (bool, error)
	Logs(ctx context.Context, space, app string, opts LogOptions, out io.Writer) error
	// Evidence collects why an app is unhealthy right now.
	Evidence(ctx context.Context, space, app string) Evidence

	// Deploy takes a built image live — storage, services, the app, its
	// addresses — narrated through progress. An app that never becomes
	// healthy fails with *RolloutError.
	Deploy(ctx context.Context, spec DeploySpec, progress Progress) error
	// Render is what Deploy would create, as text, without touching the
	// runtime. Credentials are left out: this lands in CI logs.
	Render(spec DeploySpec) (string, error)
	// Destroy removes the app and everything donkey created for it; keepData
	// spares the disk (and the data of services only this app used).
	Destroy(ctx context.Context, space, app string, keepData bool) error

	// --- Env --------------------------------------------------------------

	GetEnv(ctx context.Context, space, app string) (map[string]string, error)
	SetEnv(ctx context.Context, space, app string, env map[string]string) error

	// --- App metadata -----------------------------------------------------

	// AppMeta keeps small records on a deployed app that survive deploys. A
	// missing app or key reads as ""; writing nil removes the key.
	AppMeta(ctx context.Context, space, app, key string) (string, error)
	SetAppMeta(ctx context.Context, space, app, key string, value *string) error

	// --- Network ----------------------------------------------------------

	// Network reads actual runtime state, never stored intent. An undeployed
	// app simply has no routes.
	Network(ctx context.Context, space, app string) (*NetworkState, error)
	// ApplyNetwork changes how the app is reached, without a deploy. Existing
	// shared-address routes keep serving and follow the app's exposure. A
	// non-empty note is worth showing ("the certificate is still being issued").
	ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (note string, err error)
	// SetAccess takes effect within seconds; no deploy, no restart.
	SetAccess(ctx context.Context, space, app string, access *config.AccessConfig) error

	// --- Services (addons) ------------------------------------------------

	// ServiceBinding is the env contract of a service instance: the space's
	// own for owner == addons.SharedOwner, an app's copy otherwise.
	ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error)
	// EnsureService creates the service if missing (waiting until it serves)
	// or re-applies it. Version and storage echo what the instance uses.
	EnsureService(ctx context.Context, space string, spec ServiceSpec) (binding map[string]string, version, storage string, err error)
	// AttachService wires the binding into the app's env. Restarted reports
	// whether a running app picked it up on the spot.
	AttachService(ctx context.Context, space, app, name string, binding map[string]string) (restarted bool, err error)
	// DetachService unwires the binding. The service and its data stay.
	DetachService(ctx context.Context, space, app, name string) error
	// RemoveService detaches every app that uses the service first, and
	// returns who was detached.
	RemoveService(ctx context.Context, space, name string, keepData bool) (detached []string, err error)
	// ServiceAttachments names the apps holding a copy of the service's
	// binding.
	ServiceAttachments(ctx context.Context, space, name string) ([]string, error)
	// ServiceEndpoint is the in-runtime address, for tunnels. An empty host
	// means the service runs elsewhere.
	ServiceEndpoint(ctx context.Context, space, name string) (host string, port int32, err error)
	// ListServices lists the bindings an owner holds.
	ListServices(ctx context.Context, space, owner string) ([]ServiceBindingInfo, error)
	// SharedServiceValues collects one binding key across every space — the
	// AI proxy's token list.
	SharedServiceValues(ctx context.Context, key string) ([]string, error)

	// --- Files ------------------------------------------------------------

	// The app's disk. Paths are relative to the disk root, where each of the
	// app's storage paths has its own folder.
	ListFiles(ctx context.Context, space, app, path string) ([]FileEntry, error)
	StorageFolders(ctx context.Context, space, app string) ([]StorageFolder, error)
	ReadFile(ctx context.Context, space, app, path string, out io.Writer) error
	WriteFile(ctx context.Context, space, app, path string, in io.Reader) error
	RemoveFile(ctx context.Context, space, app, path string) error
	MoveFile(ctx context.Context, space, app, from, to string) error
	MakeDir(ctx context.Context, space, app, path string) error

	// --- Health -----------------------------------------------------------

	// Checks probes this runtime's own dependencies for doctor.
	Checks(ctx context.Context) []Check
}

// ResourceSampler is an optional runtime capability. Runtime resource usage
// is deliberately separate from Provider.Status: status is read frequently
// by fleet views, while Docker stats and the Kubernetes Metrics API are
// comparatively expensive. The server discovers this interface and samples
// it on its own bounded cadence.
type ResourceSampler interface {
	ResourceSamples(ctx context.Context) ([]ResourceSample, error)
}

// ResourceSample is one installation-owned web container at one instant. The
// telemetry wire format keeps that container identity (as the OpenTelemetry
// semantic conventions require); retained app reports aggregate replicas.
type ResourceSample struct {
	Space string
	App   string
	// Instance is the stable identity used to distinguish replicas. Container
	// metadata is carried through to OTLP's standard resource attributes.
	Instance      string
	ContainerID   string
	ContainerName string
	KubeNamespace string
	KubePodName   string
	KubePodUID    string
	SampledAt     time.Time

	CPUCores          float64
	CPUUserCores      float64
	CPUSystemCores    float64
	CPUModesAvailable bool
	MemoryBytes       int64
}

type AppRef struct {
	Space string
	App   string
}

// AppStatus is a snapshot of an app's health.
type AppStatus struct {
	Found        bool
	Image        string
	ReadyReplica int32
	WantReplica  int32
	// UpToDate: the observed state belongs to the latest generation. Without
	// it an old healthy instance makes a failing rollout look green.
	UpToDate bool
	// Instances are the app's running units, by name.
	Instances []string
	// IngressHost is the hostname the app serves on; empty for internal.
	IngressHost string
	// Protected says traffic reaches the app through the login gateway.
	Protected bool
	// Port the app listens on (donkey.yaml's `port`).
	Port int32
	// Restarts sums restarts across instances — what separates "quietly
	// struggling" from "running".
	Restarts int32
	// StartedAt is when the newest instance came up.
	StartedAt time.Time
}

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

// DeploySpec is everything Deploy and Render need.
type DeploySpec struct {
	App      *config.AppConfig
	Space    string
	ImageRef string
	// Decisions records what this deploy settled (storage, health route), so
	// a no-build deploy keeps the same shape.
	Decisions string
	// NoWait skips waiting for the app to become healthy.
	NoWait bool
}

// Progress narrates long operations back to the user, line by line.
type Progress interface {
	Line(format string, a ...any)
}

// Evidence is what donkey collects when an app is unhealthy.
type Evidence struct {
	// Summaries describe the app's instances, one line each.
	Summaries []string
	// Events are the runtime's warnings about them.
	Events []string
	// Logs are the last lines from the most broken instance.
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

// UserError marks a caller's mistake, so handlers answer 400.
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

// NetworkState is how an app is reached, read from actual state.
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
	// External: nothing is provisioned, the binding comes from URL.
	External bool
	URL      string
}

// ServiceBindingInfo is one binding as the console may see it — key names,
// never values.
type ServiceBindingInfo struct {
	Name string
	Keys []string
}

// StorageFolder maps a folder on the disk to where the app mounts it.
type StorageFolder struct {
	// Name is the folder on the disk.
	Name string
	// Path is where the app has it mounted, e.g. /app/data.
	Path string
}

type FileEntry struct {
	Name string
	Size int64
	// Mode is the ls-style spelling, e.g. "-rw-rw-r--".
	Mode    string
	ModTime time.Time
	IsDir   bool
	// IsLink: neither downloadable nor openable — callers must offer neither.
	IsLink bool
}

type Check struct {
	Name   string
	OK     bool
	Detail string
}

// App metadata keys, stored with the deployed app and surviving deploys.
const (
	// MetaConsole is written by the console endpoints only — a deploy must
	// never wipe it.
	MetaConsole = "donkey.dev/console"
	// MetaDecisions is what deploy-time analysis settled.
	MetaDecisions = "donkey.dev/decisions"
	// MetaRelease is the bounded latest deploy record rendered by the console.
	MetaRelease = "donkey.dev/release"
)

// File-browser limits, shared by every provider.
var (
	MaxUploadBytes int64 = 200 << 20
	MaxListEntries       = 2000
)

// File-browser errors, shared so handlers can map them to honest statuses.
var (
	ErrNoDisk      = errors.New("this app has no disk — add storage.paths to donkey.yaml and deploy again to give it one")
	ErrNotDeployed = errors.New("nothing with this name is deployed — deploy the app first")
	ErrBadPath     = errors.New("that path isn't allowed")
	ErrNotFound    = errors.New("there is nothing at that path on the disk")
	ErrNotAFile    = errors.New("that is a folder, not a file")
	ErrNotAFolder  = errors.New("that is a file, not a folder")
	ErrTooLarge    = errors.New("that file is too large to upload")
)
