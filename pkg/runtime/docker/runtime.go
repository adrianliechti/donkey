package docker

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	// Docker recommends reverse-DNS label keys for third-party tools. Keep
	// the same identity dimensions as Kubernetes (donkey.local/*), expressed
	// in Docker's dot-separated spelling.
	labelManaged      = "local.donkey.managed"
	labelInstallation = "local.donkey.installation"
	labelSpace        = "local.donkey.space"
	labelApp          = "local.donkey.app"
	labelAddon        = "local.donkey.addon"
	labelRole         = "local.donkey.role"
	labelImage        = "local.donkey.image"
	labelPort         = "local.donkey.port"
	labelHealthPath   = "local.donkey.health-path"
	labelHost         = "local.donkey.host"
	labelMode         = "local.donkey.mode"
	labelCORS         = "local.donkey.cors"
	labelStack        = "local.donkey.stack"

	dockerInstallation = "donkey"

	roleWorkload = "workload"
	roleAddon    = "addon"
	roleFiles    = "files"
	roleInit     = "init"
	roleSpace    = "space"
	roleStorage  = "storage"

	sharedOwner = addons.SharedOwner // the space's own binding, not an app's copy
)

// System role values, shared by the direct Docker installer, generated
// Compose bundles and the runtime's own checks. One constant per resource:
// the ownership preflights compare these exactly, so a value spelled twice
// is a re-install that refuses its own resources.
const (
	RoleServer           = "server"
	RoleOrchestrator     = "orchestrator"
	RoleGateway          = "gateway"
	RoleAI               = "ai"
	RoleProxy            = "proxy"
	RoleTelemetry        = "telemetry"
	RolePackages         = "packages"
	RoleBuildkit         = "buildkitd"
	RoleRegistry         = "registry"
	RoleNetwork          = "network"
	RoleStateStorage     = "state-storage"
	RoleBuildkitStorage  = "buildkit-storage"
	RoleTelemetryStorage = "telemetry-storage"
	RoleRegistryStorage  = "registry-storage"
)

// IdentityLabelKeys is the immutable ownership vocabulary in comparison
// order — what sameIdentity checks, exported for generated bundles whose
// shell preflight re-implements that comparison with `docker inspect`.
func IdentityLabelKeys() []string {
	return []string{labelManaged, labelInstallation, labelSpace, labelApp, labelAddon, labelRole}
}

// SystemRoleOf is the naming rule for control-plane containers: the
// container donkey-<role> carries that role label. The constants
// above and every donkey-* container name are chosen to satisfy it.
func SystemRoleOf(containerName string) string {
	return strings.TrimPrefix(containerName, "donkey-")
}

// Options is what the Docker runtime needs to know about the installation.
type Options struct {
	// Domain serves apps as <app>.<space>.<domain>.
	Domain string
	// StateDir is where the provider keeps its records (a named volume
	// shared with the gateway, which reads the routes and keeps its
	// certificates there; default /var/lib/donkey).
	StateDir string
	// Registry is where builds are pushed (as the build engine reaches it).
	Registry config.Registry
	// AddonImages are direct repository overrides for managed services.
	AddonImages     config.InstallAddonImages
	CommonOfferings config.CommonOfferings
	// BusyboxImage runs file browsing and volume initialization helpers.
	BusyboxImage string
	// PullRegistry is the same registry as the Docker daemon reaches it
	// (e.g. 127.0.0.1:5000). Image references are rewritten from the push
	// host to this one before running. Empty means refs run as pushed.
	PullRegistry string
	// AIContainer is the openai addon's shared endpoint.
	AIContainer string
	// ProxyContainer is the shared endpoint for configured HTTP offerings.
	ProxyContainer string
	// TelemetryContainer accepts app-scoped OTLP metrics on every space
	// network.
	TelemetryContainer string
	// ServerContainer/OrchestratorContainer/GatewayContainer name donkey's
	// own containers, so the provider can attach them to every space network
	// (the server must reach apps for tunnels, the orchestrator to probe a
	// deploy's readiness, the gateway to proxy).
	ServerContainer       string
	OrchestratorContainer string
	GatewayContainer      string
	// TelemetryKey derives app-scoped ingestion tokens.
	TelemetryKey []byte
}

// Runtime runs donkey's apps on a Docker engine.
type Runtime struct {
	serviceLocks addons.InstanceLocks
	cli          *client.Client
	opts         Options
	state        state
}

var _ rt.Provider = (*Runtime)(nil)

// New connects to the Docker engine (socket or DOCKER_HOST) and returns the
// runtime.
func New(opts Options) (*Runtime, error) {
	if err := config.ValidateBusyboxImage(opts.BusyboxImage); err != nil {
		return nil, err
	}
	if opts.StateDir == "" {
		opts.StateDir = "/var/lib/donkey"
	}
	if opts.ServerContainer == "" {
		opts.ServerContainer = "donkey-server"
	}
	if opts.OrchestratorContainer == "" {
		opts.OrchestratorContainer = "donkey-orchestrator"
	}
	if opts.GatewayContainer == "" {
		opts.GatewayContainer = "donkey-gateway"
	}
	if opts.AIContainer == "" {
		opts.AIContainer = "donkey-ai"
	}
	if opts.ProxyContainer == "" {
		opts.ProxyContainer = "donkey-proxy"
	}
	if opts.TelemetryContainer == "" {
		opts.TelemetryContainer = "donkey-telemetry"
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Runtime{cli: cli, opts: opts, state: state{dir: opts.StateDir}}, nil
}

// Client exposes the underlying Docker client for Docker-specific callers
// (the installer).
func (r *Runtime) Client() *client.Client { return r.cli }

// ownContainers are donkey's own containers that join every space network.
// A name left unset is one this provider has no container for.
func (r *Runtime) ownContainers() []string {
	var names []string
	for _, name := range []string{r.opts.ServerContainer, r.opts.OrchestratorContainer, r.opts.GatewayContainer,
		r.opts.AIContainer, r.opts.TelemetryContainer, r.opts.ProxyContainer} {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// --- Naming ---------------------------------------------------------------

// netName is a space's network: the apps of one space share it and resolve
// each other by name.
func netName(space string) string { return "donkey-" + space }

// ctrName is one container's name. The dots keep donkey's own containers
// (donkey-server, donkey-gateway, …) out of the namespace.
func ctrName(space, name string) string { return "donkey." + space + "." + name }

// volName is an app's (or service's) data volume.
func volName(space, name string) string { return ctrName(space, name) + ".data" }

func appLabels(space, app, role string) map[string]string {
	labels := resourceLabels(space, role)
	labels[labelApp] = app
	return labels
}

func addonLabels(space, addon, role string) map[string]string {
	labels := resourceLabels(space, role)
	labels[labelAddon] = addon
	return labels
}

// SystemLabels is the ownership/role vocabulary shared by the direct Docker
// installer, generated Compose bundles, and runtime-created resources.
func SystemLabels(role string) map[string]string {
	return map[string]string{
		labelManaged:      "true",
		labelInstallation: dockerInstallation,
		labelRole:         role,
	}
}

func resourceLabels(space, role string) map[string]string {
	labels := SystemLabels(role)
	labels[labelSpace] = space
	return labels
}

// sameIdentity compares only immutable ownership dimensions. Version, image,
// port and other descriptive labels may legitimately change on a rollout.
func sameIdentity(actual, expected map[string]string) bool {
	for _, key := range IdentityLabelKeys() {
		if actual[key] != expected[key] {
			return false
		}
	}
	return true
}

// MatchesSystemLabels reports whether labels describe exactly one donkey
// control-plane resource. Tenant dimensions must be absent: an app resource
// does not become installation infrastructure merely by carrying the three
// system labels as well.
func MatchesSystemLabels(actual map[string]string, role string) bool {
	return sameIdentity(actual, SystemLabels(role))
}

func requireIdentity(kind, name string, actual, expected map[string]string) error {
	if sameIdentity(actual, expected) {
		return nil
	}
	return fmt.Errorf("the Docker %s %s exists but isn't the expected donkey resource — remove or rename it before continuing", kind, name)
}

func (r *Runtime) removeOwnedContainer(ctx context.Context, name string, expected map[string]string) error {
	info, err := r.cli.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Config == nil {
		return fmt.Errorf("the Docker container %s has no inspectable configuration", name)
	}
	if err := requireIdentity("container", name, info.Config.Labels, expected); err != nil {
		return err
	}
	// Names are reusable. Remove the immutable ID that passed the ownership
	// check so a concurrent same-named replacement cannot be deleted instead.
	target := info.ID
	if target == "" {
		target = name // defensive fallback for unusual Engine-compatible APIs
	}
	return r.removeContainer(ctx, target)
}

func (r *Runtime) ensureOwnedVolume(ctx context.Context, name string, expected map[string]string) error {
	vol, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: expected})
	if err != nil {
		return err
	}
	return requireIdentity("volume", name, vol.Labels, expected)
}

func (r *Runtime) removeOwnedVolume(ctx context.Context, name string, expected map[string]string) error {
	vol, err := r.cli.VolumeInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := requireIdentity("volume", name, vol.Labels, expected); err != nil {
		return err
	}
	err = r.cli.VolumeRemove(ctx, name, false)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// pullRef rewrites an image reference from the push registry's host to the
// one the Docker daemon reaches.
func (r *Runtime) pullRef(imageRef string) string {
	if r.opts.PullRegistry == "" {
		return imageRef
	}
	push := r.opts.Registry.Host()
	if push != "" && strings.HasPrefix(imageRef, push+"/") {
		return r.opts.PullRegistry + strings.TrimPrefix(imageRef, push)
	}
	return imageRef
}

// pushRef is pullRef backwards: the image as donkey named it when it built
// it, which is how build records and rollbacks know it. Containers carry the
// pull spelling, so what Status reports is translated back.
func (r *Runtime) pushRef(imageRef string) string {
	push := r.opts.Registry.Host()
	if r.opts.PullRegistry == "" || push == "" {
		return imageRef
	}
	if strings.HasPrefix(imageRef, r.opts.PullRegistry+"/") {
		return push + strings.TrimPrefix(imageRef, r.opts.PullRegistry)
	}
	return imageRef
}

// --- Spaces ---------------------------------------------------------------

func (r *Runtime) ValidateSpace(space string) error {
	if err := config.ValidateSpaceName(space); err != nil {
		return err
	}
	return nil
}

func (r *Runtime) CheckSpaceFree(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	nw, err := r.cli.NetworkInspect(ctx, netName(space), network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
		return fmt.Errorf("the network %s exists but isn't donkey's — pick a different space name", netName(space))
	}
	return nil
}

// ensureSpace creates the space's network and attaches donkey's own
// containers to it: the gateway to proxy, the server to tunnel, the
// orchestrator to probe the apps it deploys.
func (r *Runtime) ensureSpace(ctx context.Context, space string) error {
	if err := r.CheckSpaceFree(ctx, space); err != nil {
		return err
	}
	own, err := ownedSystemContainers(ctx, r.cli, r.ownContainers()...)
	if err != nil {
		return err
	}
	name := netName(space)
	nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		_, err = r.cli.NetworkCreate(ctx, name, network.CreateOptions{
			Labels: resourceLabels(space, roleSpace),
		})
		if err == nil {
			err = r.state.writeRecord(config.DefaultSpaceConfig(space, r.opts.Domain), "spaces", space, "config")
		}
		if err == nil {
			nw, err = r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
		}
	}
	if err != nil {
		return err
	}
	if err := r.recordProxyNetwork(space, nw.IPAM); err != nil {
		return err
	}
	for _, containerName := range own {
		err := r.cli.NetworkConnect(ctx, name, containerName, nil)
		// Already attached is the normal idempotent case.
		if err != nil && !strings.Contains(err.Error(), "already exists") && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Runtime) ListSpaces(ctx context.Context) ([]string, error) {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelRole+"="+roleSpace),
		),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, nw := range nets {
		if space := nw.Labels[labelSpace]; space != "" && r.ValidateSpace(space) == nil &&
			sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
			out = append(out, space)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *Runtime) CreateSpace(ctx context.Context, space string) error {
	return r.ensureSpace(ctx, space)
}

func (r *Runtime) DeleteSpace(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	// A label on an arbitrary same-named network never grants deletion
	// authority unless its logical space matches too.
	nw, inspectErr := r.cli.NetworkInspect(ctx, netName(space), network.InspectOptions{})
	if inspectErr != nil && !cerrdefs.IsNotFound(inspectErr) {
		return inspectErr
	}
	if inspectErr == nil && !sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
		return fmt.Errorf("the network %s isn't donkey's space %s to delete", netName(space), space)
	}
	if err := r.deprovisionSpacePlugins(ctx, space); err != nil {
		return err
	}
	own, err := ownedSystemContainers(ctx, r.cli, r.ownContainers()...)
	if err != nil {
		return err
	}
	// Apps, managed services, and short-lived helpers all go with the space.
	resources, err := r.spaceContainers(ctx, space, "")
	if err != nil {
		return err
	}
	for _, c := range resources {
		if err := r.removeContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	// Reported, not swallowed: a space whose volumes survived would be
	// reported deleted, and the next space of the same name would reuse them
	// with credentials that no longer match its bindings.
	vols, err := r.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelSpace+"="+space),
		),
	})
	if err != nil {
		return fmt.Errorf("couldn't list the data volumes of space %s: %w", space, err)
	}
	for _, v := range vols.Volumes {
		if err := r.cli.VolumeRemove(ctx, v.Name, false); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("couldn't remove the data volume %s: %w", v.Name, err)
		}
	}
	if err := r.state.cleanupSpace(space); err != nil {
		return err
	}
	if err := r.state.remove("proxy", "networks", space); err != nil {
		return err
	}
	// donkey's own containers were attached to the network (to proxy and to
	// probe) — it only goes once they let it.
	for _, containerName := range own {
		_ = r.cli.NetworkDisconnect(ctx, netName(space), containerName, true)
	}
	if inspectErr != nil {
		return nil
	}
	err = r.cli.NetworkRemove(ctx, netName(space))
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// spaceContainers lists a space's containers, all or one Donkey role.
func (r *Runtime) spaceContainers(ctx context.Context, space, role string) ([]container.Summary, error) {
	args := filters.NewArgs(
		filters.Arg("label", labelManaged+"=true"),
		filters.Arg("label", labelInstallation+"="+dockerInstallation),
		filters.Arg("label", labelSpace+"="+space),
	)
	if role != "" {
		args.Add("label", labelRole+"="+role)
	}
	return r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
}

func (r *Runtime) removeContainer(ctx context.Context, id string) error {
	err := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// --- Apps -----------------------------------------------------------------

func (r *Runtime) ListApps(ctx context.Context) ([]rt.AppRef, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelRole+"="+roleWorkload),
		),
	})
	if err != nil {
		return nil, err
	}
	var out []rt.AppRef
	seen := map[string]bool{}
	for _, c := range list {
		space, app := c.Labels[labelSpace], c.Labels[labelApp]
		if space == "" || app == "" || !sameIdentity(c.Labels, appLabels(space, app, roleWorkload)) {
			continue
		}
		// A stopped previous container can coexist with its candidate until
		// readiness succeeds. They are versions of one app.
		key := space + "/" + app
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, rt.AppRef{Space: space, App: app})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Space != out[j].Space {
			return out[i].Space < out[j].Space
		}
		return out[i].App < out[j].App
	})
	return out, nil
}

func (r *Runtime) Status(ctx context.Context, space, app string) (*rt.AppStatus, error) {
	st := &rt.AppStatus{}
	info, err := r.cli.ContainerInspect(ctx, ctrName(space, app))
	if cerrdefs.IsNotFound(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Config == nil {
		return nil, fmt.Errorf("the Docker container %s has no inspectable configuration", ctrName(space, app))
	}
	if err := requireIdentity("container", ctrName(space, app), info.Config.Labels,
		appLabels(space, app, roleWorkload)); err != nil {
		return nil, err
	}
	st.Found = true
	st.UpToDate = true
	st.WantReplica = 1
	st.Image = info.Config.Labels[labelImage]
	if st.Image == "" {
		st.Image = info.Config.Image
	}
	st.Image = r.pushRef(st.Image)
	st.Stack = info.Config.Labels[labelStack]
	if p, err := strconv.Atoi(info.Config.Labels[labelPort]); err == nil {
		st.Port = int32(p)
	}
	st.Mode = info.Config.Labels[labelMode]
	st.CORS = info.Config.Labels[labelCORS]
	if spec, err := r.runRecord(space, app); err == nil && spec != nil {
		st.Mode = spec.Mode
		st.CORS = spec.CORS
	}
	st.Restarts = int32(info.RestartCount)
	if info.State != nil {
		if info.State.Running && !info.State.Restarting && r.probeContainer(ctx, info) == nil {
			st.ReadyReplica = 1
		}
		if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil && t.Unix() > 0 {
			st.StartedAt = t
		}
		st.Instances = append(st.Instances, containerSummary(info))
	}
	if route, err := r.state.route(space, app); err == nil && route != nil {
		st.PublicHost = route.Host
		st.Protected = route.Login
		st.Mode = route.Mode
		st.CORS = route.CORS
	}
	return st, nil
}

// containerSummary is one line about the app's container, for status and
// evidence.
func containerSummary(info container.InspectResponse) string {
	name := strings.TrimPrefix(info.Name, "/")
	s := info.State
	switch {
	case s == nil:
		return name
	case s.Running:
		return fmt.Sprintf("%s: running (restarts %d)", name, info.RestartCount)
	case s.OOMKilled:
		return fmt.Sprintf("%s: killed — out of memory", name)
	default:
		return fmt.Sprintf("%s: %s (exit code %d)", name, s.Status, s.ExitCode)
	}
}

func (r *Runtime) HasData(ctx context.Context, space, app string) bool {
	vol, err := r.cli.VolumeInspect(ctx, volName(space, app))
	return err == nil && sameIdentity(vol.Labels, appLabels(space, app, roleStorage))
}

// Restart recreates the app's container from its stored run record — that
// is what makes changed env and bindings take effect, exactly like a
// Kubernetes rollout re-reading its secrets.
func (r *Runtime) Restart(ctx context.Context, space, app string) (bool, error) {
	spec, err := r.runRecord(space, app)
	if err != nil {
		return false, err
	}
	if spec == nil {
		return false, nil
	}
	if err := r.runApp(ctx, space, app, *spec); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Runtime) Logs(ctx context.Context, space, app string, opts rt.LogOptions, out io.Writer) error {
	name := ctrName(space, app)
	expected := appLabels(space, app, roleWorkload)
	// A space's services are not apps; their container carries the
	// service's own name.
	//
	// The name is checked here as well as at the handler because it ends up in
	// the engine client's request URL, which path.Joins it — an unchecked
	// "x/../.." would collapse out of the containers endpoint and read any
	// container on the host.
	if svc, ok := strings.CutPrefix(opts.Component, "addon-"); ok {
		if !config.ValidName(svc) {
			return fmt.Errorf("%q is not a service of this space", svc)
		}
		name = ctrName(space, svc)
		expected = addonLabels(space, svc, roleAddon)
	}
	info, err := r.cli.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("nothing with this name is deployed")
	}
	if err != nil {
		return err
	}
	if info.Config == nil {
		return fmt.Errorf("the Docker container %s has no inspectable configuration", name)
	}
	if err := requireIdentity("container", name, info.Config.Labels, expected); err != nil {
		return err
	}
	lo := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       strconv.FormatInt(opts.TailLines, 10),
	}
	if opts.Since > 0 {
		lo.Since = time.Now().Add(-opts.Since).Format(time.RFC3339)
	}
	rc, err := r.cli.ContainerLogs(ctx, name, lo)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("nothing with this name is deployed")
		}
		return err
	}
	defer rc.Close()
	_, err = stdcopy.StdCopy(out, out, rc)
	if ctx.Err() != nil {
		return nil // a cancelled follow is a normal hang-up
	}
	return err
}

func (r *Runtime) Evidence(ctx context.Context, space, app string) rt.Evidence {
	ev := rt.Evidence{}
	info, err := r.cli.ContainerInspect(ctx, ctrName(space, app))
	if err != nil {
		return ev
	}
	if info.Config == nil || !sameIdentity(info.Config.Labels, appLabels(space, app, roleWorkload)) {
		return ev
	}
	ev.Summaries = append(ev.Summaries, containerSummary(info))
	if info.State != nil && info.State.Error != "" {
		ev.Events = append(ev.Events, info.State.Error)
	}
	ev.Logs = r.logsTail(ctx, ctrName(space, app), 100)
	return ev
}

func (r *Runtime) logsTail(ctx context.Context, name string, lines int) string {
	rc, err := r.cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines),
	})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var b strings.Builder
	_, _ = stdcopy.StdCopy(&b, &b, rc)
	return b.String()
}

// --- Env ------------------------------------------------------------------

func (r *Runtime) GetEnv(ctx context.Context, space, app string) (map[string]string, error) {
	return r.state.env(space, app)
}

func (r *Runtime) SetEnv(ctx context.Context, space, app string, env map[string]string) error {
	return r.state.setEnv(space, app, env)
}

// --- App metadata ---------------------------------------------------------

func (r *Runtime) AppMeta(ctx context.Context, space, app, key string) (string, error) {
	m, err := r.state.meta(space, app)
	if err != nil {
		return "", err
	}
	return m[key], nil
}

func (r *Runtime) SetAppMeta(ctx context.Context, space, app, key string, value *string) error {
	m, err := r.state.meta(space, app)
	if err != nil {
		return err
	}
	if value == nil {
		delete(m, key)
	} else {
		m[key] = *value
	}
	return r.state.setMeta(space, app, m)
}

// --- Services (addons) ----------------------------------------------------

func (r *Runtime) ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error) {
	binding, err := r.state.binding(space, owner, name)
	if err != nil {
		return nil, err
	}
	if state, err := r.pluginState(space, name); err != nil {
		return nil, err
	} else if state != nil {
		return binding, nil // plugin addresses are already app-reachable
	}
	// Older installations stored short network aliases. The server belongs
	// to every space network, where those aliases are ambiguous. Resolve an
	// existing managed service by its owned container name on read as well.
	scoped := rewriteBindingHost(binding, name, ctrName(space, name))
	if maps.Equal(binding, scoped) {
		return binding, nil
	}
	host, _, err := r.ServiceEndpoint(ctx, space, name)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return binding, nil
	} // external binding
	return scoped, nil
}

func (r *Runtime) ServiceConnection(ctx context.Context, space, name string) (rt.ServiceConnection, error) {
	binding, err := r.ServiceBinding(ctx, space, sharedOwner, name)
	if err != nil {
		return rt.ServiceConnection{}, err
	}
	metadata, err := r.serviceMetadata(space, name, binding)
	if err != nil {
		return rt.ServiceConnection{}, err
	}
	return rt.ServiceConnection{Binding: binding, Type: metadata.Type, Protocol: metadata.Protocol, Parameters: metadata.Parameters}, nil
}

func (r *Runtime) serviceMetadata(space, name string, binding map[string]string) (addons.Metadata, error) {
	var metadata addons.Metadata
	_, err := r.state.readRecord(&metadata, "service-metadata", space, name)
	return metadata.Resolve(binding), err
}

func (r *Runtime) aiProxyURL() string {
	return fmt.Sprintf("http://%s:8089/v1", r.opts.AIContainer)
}

func (r *Runtime) EnsureService(ctx context.Context, space string, spec rt.ServiceSpec) (map[string]string, map[string]any, error) {
	unlock := r.serviceLocks.Lock(space, spec.Name)
	defer unlock()
	if err := config.ValidateServiceName(spec.Name); err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	spec.Type = config.NormalizeAddonType(spec.Type)
	// The record comes first: an instance a provider created resolves
	// through its recorded offering when the provider is not answering.
	previous, err := r.pluginState(space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	if previous != nil && previous.Type != spec.Type {
		return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
	}
	addon, err := addons.Lookup(ctx, spec.Type, previous)
	if err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	desc, err := addon.Describe(ctx)
	if err != nil {
		return nil, nil, err
	}
	connection, err := r.ServiceConnection(ctx, space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	if err := (addons.Metadata{Type: connection.Type, Protocol: connection.Protocol}).CheckType(spec.Type); err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	spec.Parameters, err = rt.PrepareService(ctx, r, r.opts.CommonOfferings, space, desc, spec.Parameters, previous != nil || len(connection.Binding) > 0)
	if err != nil {
		return nil, nil, err
	}
	if err := r.ensureSpace(ctx, space); err != nil {
		return nil, nil, err
	}

	plugin, isPlugin := addon.(*addons.Plugin)
	builtin, _ := addon.(addons.Builtin)
	if previous != nil && (!isPlugin || spec.External) {
		return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
	}
	if isPlugin && spec.External {
		return nil, nil, rt.UserError{Err: fmt.Errorf("the %s service is provisioned by provider %s; external URL bindings are not supported for it", spec.Type, plugin.Provider())}
	}
	var binding map[string]string
	var params addons.Parameters
	switch {
	case isPlugin && !spec.External:
		binding, params, err = r.provisionPlugin(ctx, space, spec, plugin, desc, previous)
		if err != nil {
			return nil, nil, err
		}
	case spec.External:
		binding, err = builtin.ExternalBinding(spec.URL)
		if err != nil {
			return nil, nil, rt.UserError{Err: err}
		}
		if err := r.state.setBinding(space, sharedOwner, spec.Name, binding); err != nil {
			return nil, nil, err
		}
	default:
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// already holds.
		if info, inspectErr := r.cli.ContainerInspect(ctx, ctrName(space, spec.Name)); inspectErr == nil {
			if info.Config == nil {
				return nil, nil, fmt.Errorf("the Docker container %s has no inspectable configuration", ctrName(space, spec.Name))
			}
			switch {
			case sameIdentity(info.Config.Labels, appLabels(space, spec.Name, roleWorkload)):
				return nil, nil, rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
			case !sameIdentity(info.Config.Labels, addonLabels(space, spec.Name, roleAddon)):
				return nil, nil, requireIdentity("container", ctrName(space, spec.Name), info.Config.Labels,
					addonLabels(space, spec.Name, roleAddon))
			}
		} else if !cerrdefs.IsNotFound(inspectErr) {
			return nil, nil, inspectErr
		}
		// An existing instance keeps its credential and, unless asked
		// otherwise, its parameters.
		params, err = addons.ResolveInstanceParameters(desc, spec.Parameters, addons.Parameters(connection.Parameters))
		if err != nil {
			return nil, nil, rt.UserError{Err: err}
		}
		in := addons.Input{
			Space: space, Namespace: netName(space), Name: spec.Name,
			ImageRepository: r.opts.AddonImages.Repository(spec.Type),
			AIProxyURL:      r.aiProxyURL(),
			ServiceProxyURL: r.serviceProxyURL(),
			ExaProxyURL:     r.exaProxyURL(),
		}.WithParameters(params)
		// Reuse the credential the instance was initialized with — rotating
		// it here would lock the service out of its own kept data.
		if pw, ok := connection.Binding[builtin.CredentialKey()]; ok {
			in.Password = pw
		} else if in.Password, err = addons.NewPassword(); err != nil {
			return nil, nil, err
		}
		binding = builtin.Binding(in)
		// The unique container name works for both apps in this space and
		// the admin server, which belongs to multiple space networks.
		binding = rewriteBindingHost(binding, in.Name+"."+in.Namespace+".svc", ctrName(space, in.Name))
		if err := r.state.setBinding(space, sharedOwner, spec.Name, binding); err != nil {
			return nil, nil, err
		}
		workload, err := builtin.Workload(in)
		if err != nil {
			return nil, nil, err
		}
		if workload != nil {
			if err := r.runService(ctx, space, spec.Name, in, workload, binding); err != nil {
				return nil, nil, err
			}
		}
	}
	if !isPlugin || spec.External {
		if err := r.state.writeRecord(addons.MetadataFor(desc, params), "service-metadata", space, spec.Name); err != nil {
			return nil, nil, err
		}
	}
	// Attached apps hold copies; refresh them (and restart on external
	// changes, whose addresses may have moved).
	attached, err := r.state.attachments(space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	for _, app := range attached {
		if err := r.state.setBinding(space, app, spec.Name, binding); err != nil {
			return nil, nil, err
		}
		if spec.External || isPlugin {
			if _, err := r.Restart(ctx, space, app); err != nil {
				return nil, nil, err
			}
		}
	}
	return binding, params, nil
}

// Rewrite address fields only; passwords, usernames and URL paths are data.
func rewriteBindingHost(binding map[string]string, from, to string) map[string]string {
	out := maps.Clone(binding)
	for key, value := range binding {
		switch key {
		case "PGHOST", "REDIS_HOST":
			if value == from {
				out[key] = to
			}
		case "REDIS_ADDR":
			if host, port, err := net.SplitHostPort(value); err == nil && host == from {
				out[key] = net.JoinHostPort(to, port)
			}
		case "DATABASE_URL", "JDBC_DATABASE_URL", "REDIS_URL", "AWS_ENDPOINT_URL_S3", "S3_URL", "OPENAI_BASE_URL", "EXA_BASE_URL":
			prefix := ""
			if strings.HasPrefix(value, "jdbc:") {
				prefix, value = "jdbc:", strings.TrimPrefix(value, "jdbc:")
			}
			u, err := url.Parse(value)
			if err != nil || u.Hostname() != from {
				continue
			}
			port := u.Port()
			u.Host = to
			if port != "" {
				u.Host = net.JoinHostPort(to, port)
			}
			out[key] = prefix + u.String()
		}
	}
	return out
}

// runService materializes an addon workload as a container with its data
// volume, and waits until it runs.
func (r *Runtime) runService(ctx context.Context, space, name string, in addons.Input, w *addons.Workload, binding map[string]string) error {
	labels := addonLabels(space, name, roleAddon)
	labels[labelPort] = strconv.Itoa(int(w.Port))

	vol := volName(space, name)
	if err := r.ensureOwnedVolume(ctx, vol, addonLabels(space, name, roleStorage)); err != nil {
		return err
	}
	env := map[string]string{}
	for k, v := range w.Env {
		env[k] = v
	}
	// Docker has no secret references — the values go in directly.
	for envName, bindingKey := range w.SecretEnv {
		env[envName] = binding[bindingKey]
	}
	if err := r.replaceContainer(ctx, ctrName(space, name), runConfig{
		Entrypoint: w.Command,
		Image:      w.Image,
		Env:        env,
		Labels:     labels,
		Network:    netName(space),
		Alias:      name,
		Volumes:    map[string]volumeMount{w.DataPath: {Volume: vol}},
	}); err != nil {
		return err
	}
	return r.waitRunning(ctx, ctrName(space, name), 120*time.Second)
}

func (r *Runtime) AttachService(ctx context.Context, space, app, name string, binding map[string]string) (bool, error) {
	if err := rt.CheckServiceAttachment(ctx, r, r.opts.CommonOfferings, space, app, name); err != nil {
		return false, err
	}
	if err := r.state.setBinding(space, app, name, binding); err != nil {
		return false, err
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	return r.Restart(ctx, space, app)
}

// DetachService drops the app's copy of a binding and every env reference to
// it — the stored ones, and donkey.yaml's in the run record as Kubernetes
// strips them from the Deployment — so the restart does not trip over them.
func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	env, err := r.state.env(space, app)
	if err != nil {
		return err
	}
	if dropReferences(env, name) {
		if err := r.state.setEnv(space, app, env); err != nil {
			return err
		}
	}
	spec, err := r.runRecord(space, app)
	if err != nil {
		return err
	}
	if spec != nil && dropReferences(spec.Env, name) {
		if err := r.setRunRecord(space, app, *spec); err != nil {
			return err
		}
	}
	if err := r.state.removeBinding(space, app, name); err != nil {
		return err
	}
	_, err = r.Restart(ctx, space, app)
	return err
}

// dropReferences deletes the env entries that reference one service.
func dropReferences(env map[string]string, service string) bool {
	dropped := false
	for key, value := range env {
		if name, _, ref := config.ParseEnvRef(value); ref && name == service {
			delete(env, key)
			dropped = true
		}
	}
	return dropped
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData, force bool) ([]string, error) {
	unlock := r.serviceLocks.Lock(space, name)
	defer unlock()
	if keepData {
		connection, err := r.ServiceConnection(ctx, space, name)
		if err != nil {
			return nil, err
		}
		if err := (addons.Metadata{Type: connection.Type, Protocol: connection.Protocol}).CheckKeepData(ctx, keepData); err != nil {
			return nil, rt.UserError{Err: err}
		}
		if addons.IsProxyBinding(r.serviceProxyURL(), connection.Type, connection.Binding) {
			return nil, rt.UserError{Err: fmt.Errorf("proxy services have no stored data; remove without --keep-data to revoke access")}
		}
	}
	attached, err := r.state.attachments(space, name)
	if err != nil {
		return nil, err
	}
	if err := r.deprovisionPlugin(ctx, space, name, keepData, force); err != nil {
		return nil, err
	}
	for _, app := range attached {
		if err := r.DetachService(ctx, space, app, name); err != nil {
			return nil, err
		}
	}
	if err := r.removeOwnedContainer(ctx, ctrName(space, name), addonLabels(space, name, roleAddon)); err != nil {
		return nil, err
	}
	if !keepData {
		if err := r.state.removeBinding(space, sharedOwner, name); err != nil {
			return nil, err
		}
		if err := r.removeOwnedVolume(ctx, volName(space, name), addonLabels(space, name, roleStorage)); err != nil {
			return nil, err
		}
		if err := r.state.remove("provisioning", space, name); err != nil {
			return nil, err
		}
		if err := r.state.remove("service-metadata", space, name); err != nil {
			return nil, err
		}
	}
	return attached, nil
}

func (r *Runtime) ServiceAttachments(ctx context.Context, space, name string) ([]string, error) {
	return r.state.attachments(space, name)
}

func (r *Runtime) ServiceEndpoint(ctx context.Context, space, name string) (string, int32, error) {
	state, err := r.pluginState(space, name)
	if err != nil {
		return "", 0, err
	}
	if state != nil {
		host, port := state.Endpoint()
		return host, port, nil
	}
	info, err := r.cli.ContainerInspect(ctx, ctrName(space, name))
	if cerrdefs.IsNotFound(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if info.Config == nil {
		return "", 0, fmt.Errorf("the Docker container %s has no inspectable configuration", ctrName(space, name))
	}
	if err := requireIdentity("container", ctrName(space, name), info.Config.Labels,
		addonLabels(space, name, roleAddon)); err != nil {
		return "", 0, err
	}
	port, _ := strconv.Atoi(info.Config.Labels[labelPort])
	if port == 0 {
		return "", 0, fmt.Errorf("the service %s exposes no port", name)
	}
	return ctrName(space, name), int32(port), nil
}

func (r *Runtime) ListServices(ctx context.Context, space, owner string) ([]rt.ServiceBindingInfo, error) {
	names, err := r.state.listBindings(space, owner)
	if err != nil {
		return nil, err
	}
	var out []rt.ServiceBindingInfo
	for _, name := range names {
		data, err := r.state.binding(space, owner, name)
		if err != nil {
			return nil, err
		}
		metadata, err := r.serviceMetadata(space, name, data)
		if err != nil {
			return nil, err
		}
		info := rt.ServiceBindingInfo{Name: name, Type: metadata.Type, Protocol: metadata.Protocol, Parameters: metadata.Parameters}
		for k := range data {
			info.Keys = append(info.Keys, k)
		}
		sort.Strings(info.Keys)
		out = append(out, info)
	}
	if owner == sharedOwner {
		// The space's own view also shows plugin instances that have no
		// binding yet (or any more): pending, failed and stopped ones.
		bound := map[string]bool{}
		for _, b := range out {
			bound[b.Name] = true
		}
		unbound, err := r.unboundPluginServices(space, bound)
		if err != nil {
			return nil, err
		}
		out = append(out, unbound...)
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	return out, nil
}

func (r *Runtime) exaProxyURL() string {
	return strings.TrimSuffix(r.aiProxyURL(), "/v1") + "/exa"
}

func (r *Runtime) ExaProxyTokens(ctx context.Context) ([]string, error) {
	return r.state.bindingValues("EXA_API_KEY", "EXA_BASE_URL", r.exaProxyURL())
}

func (r *Runtime) AIProxyTokens(ctx context.Context) ([]string, error) {
	return r.state.bindingValues("OPENAI_API_KEY", "OPENAI_BASE_URL", r.aiProxyURL())
}

func (r *Runtime) serviceProxyURL() string {
	name := r.opts.ProxyContainer
	if name == "" {
		name = "donkey-proxy"
	}
	return "http://" + name + ":8091"
}

// SetProxyServices updates the shared proxy's read-only configuration file.
func (r *Runtime) SetProxyServices(services config.ProxyServices) error {
	if err := services.Validate(); err != nil {
		return err
	}
	return r.state.writeRecord(services, "proxy", "services")
}

// --- Health ---------------------------------------------------------------

func (r *Runtime) Checks(ctx context.Context) []rt.Check {
	var checks []rt.Check

	engine := rt.Check{Name: "Docker engine"}
	if info, err := r.cli.Ping(ctx); err != nil {
		engine.Detail = err.Error()
	} else {
		engine.OK = true
		engine.Detail = "answering (API " + info.APIVersion + ")"
	}
	checks = append(checks, engine)

	gw := rt.Check{Name: "App gateway (donkey-gateway)"}
	if info, err := r.cli.ContainerInspect(ctx, r.opts.GatewayContainer); cerrdefs.IsNotFound(err) {
		gw.Detail = "not found — is the gateway container running?"
	} else if err != nil {
		gw.Detail = err.Error()
	} else if info.Config == nil || !MatchesSystemLabels(info.Config.Labels, SystemRoleOf(r.opts.GatewayContainer)) {
		gw.Detail = "the name is held by a container outside this donkey installation"
	} else if info.State == nil || !info.State.Running {
		gw.Detail = "not running"
	} else {
		gw.OK = true
		gw.Detail = "routing"
	}
	checks = append(checks, gw)

	return checks
}

// waitRunning requires a reachable endpoint over a short stable window.
// Failures retain startup evidence for the automatic repair loop.
func (r *Runtime) waitRunning(parent context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	stableSince := time.Time{}
	lastProbe := "the process has not started"
	var previous container.InspectResponse
	failed := func(reason string) error {
		if parent.Err() != nil {
			return parent.Err()
		}
		app := name
		var summaries []string
		if previous.Config != nil {
			app = cmp.Or(previous.Config.Labels[labelApp], previous.Config.Labels[labelAddon], name)
		}
		if previous.ContainerJSONBase != nil {
			summaries = []string{containerSummary(previous)}
		}
		logsCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		return &rt.RolloutError{App: app, Reason: reason, Evidence: rt.Evidence{Summaries: summaries, Logs: r.logsTail(logsCtx, name, 100)}}
	}
	for {
		info, err := r.cli.ContainerInspect(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return failed("readiness check timed out: " + lastProbe)
			}
			return err
		}
		if info.ContainerJSONBase == nil {
			return fmt.Errorf("container %s has no inspectable state", name)
		}
		if previous.ContainerJSONBase != nil && (previous.RestartCount != info.RestartCount || previous.State != nil && info.State != nil && previous.State.StartedAt != info.State.StartedAt) {
			stableSince = time.Time{}
		}
		previous = info
		state := info.State
		switch {
		case state != nil && (state.Status == "exited" || state.Restarting || state.OOMKilled):
			return failed(containerSummary(info))
		case state != nil && state.Running:
			if err := r.probeContainer(ctx, info); err != nil {
				lastProbe = err.Error()
				stableSince = time.Time{}
			} else {
				if stableSince.IsZero() {
					stableSince = time.Now()
				}
				if time.Since(stableSince) >= 3*time.Second {
					return nil
				}
			}
		default:
			stableSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return failed("readiness check timed out: " + lastProbe)
		case <-time.After(time.Second):
		}
	}
}

// AttachOwnContainers connects donkey's own containers to every space network
// that already exists.
//
// Recreating a container drops every network it was attached to, and an
// install recreates both of donkey's. Docker's embedded DNS only answers for
// networks the asking container is on, so without this a re-install leaves the
// gateway unable to resolve any app deployed before it — every existing app
// stops answering until it happens to be redeployed. ensureSpace does the same
// thing for one new space; this covers the ones already there.
func AttachOwnContainers(ctx context.Context, cli *client.Client, names ...string) error {
	owned, err := ownedSystemContainers(ctx, cli, names...)
	if err != nil {
		return err
	}
	nets, err := cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelRole+"="+roleSpace),
		),
	})
	if err != nil {
		return err
	}
	for _, nw := range nets {
		space := nw.Labels[labelSpace]
		if space == "" || !sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
			continue
		}
		for _, name := range owned {
			err := cli.NetworkConnect(ctx, nw.Name, name, nil)
			// Already attached is the normal case on a re-install that
			// changed nothing; a missing container is not this code's problem.
			if err != nil && !strings.Contains(err.Error(), "already exists") && !cerrdefs.IsNotFound(err) {
				return fmt.Errorf("couldn't attach %s to the network of space %s: %w", name, nw.Labels[labelSpace], err)
			}
		}
	}
	return nil
}

// ownedSystemContainers resolves the control-plane names that are present and
// verifies their complete immutable identity before another resource is
// connected to, disconnected from, or diagnosed through them. Missing names
// are normal when the provider runs outside Docker in development.
func ownedSystemContainers(ctx context.Context, cli *client.Client, names ...string) ([]string, error) {
	owned := make([]string, 0, len(names))
	for _, name := range names {
		info, err := cli.ContainerInspect(ctx, name)
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Config == nil {
			return nil, fmt.Errorf("the Docker container %s has no inspectable configuration", name)
		}
		role := SystemRoleOf(name)
		if !MatchesSystemLabels(info.Config.Labels, role) {
			return nil, fmt.Errorf("the Docker container %s exists but isn't this donkey installation's %s resource", name, role)
		}
		owned = append(owned, name)
	}
	return owned, nil
}

// ReconcileOwnNetworks keeps donkey's own containers attached to every space
// network for as long as ctx lives.
//
// Attaching once is not enough, because more than one thing detaches them.
// Recreating a container drops all of its networks, and an install recreates
// both — but so does anyone who removes and re-runs one by hand. A deploy that
// lands while a container is briefly absent skips it silently: ensureSpace
// treats "no such container" as fine, since donkey may be running outside
// Docker in development. And a space network removed and recreated outside
// donkey comes back with nobody on it.
//
// The gateway cannot repair this itself — it holds no Docker socket, only the
// state volume — so the server, which is the runtime provider, does it for
// both. The check costs one network list and a few no-op connects, and the
// failure it prevents is silent and total: every app in a space stops
// answering, with nothing in its own logs to say why.
func (r *Runtime) ReconcileOwnNetworks(ctx context.Context, every time.Duration) {
	own := r.ownContainers()
	for {
		if err := AttachOwnContainers(ctx, r.cli, own...); err != nil && ctx.Err() == nil {
			log.Printf("couldn't attach donkey's containers to the space networks: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
