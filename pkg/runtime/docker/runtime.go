package docker

import (
	"context"
	"fmt"
	"io"
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
	labelManaged   = "donkey.managed"
	labelSpace     = "donkey.space"
	labelApp       = "donkey.app"
	labelComponent = "donkey.component"
	labelImage     = "donkey.image"
	labelPort      = "donkey.port"

	componentWeb = "web"

	sharedOwner = "shared" // addons.SharedOwner, spelled here for state.go
)

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
	// PullRegistry is the same registry as the Docker daemon reaches it
	// (e.g. 127.0.0.1:5000). Image references are rewritten from the push
	// host to this one before running. Empty means refs run as pushed.
	PullRegistry string
	// ServerContainer/GatewayContainer name donkey's own containers, so the
	// provider can attach them to every space network (the server must
	// reach apps for tunnels and probes, the gateway to proxy).
	ServerContainer  string
	GatewayContainer string
}

// Runtime runs donkey's apps on a Docker engine.
type Runtime struct {
	cli   *client.Client
	opts  Options
	state state
}

var _ rt.Provider = (*Runtime)(nil)

// New connects to the Docker engine (socket or DOCKER_HOST) and returns the
// runtime.
func New(opts Options) (*Runtime, error) {
	if opts.StateDir == "" {
		opts.StateDir = "/var/lib/donkey"
	}
	if opts.ServerContainer == "" {
		opts.ServerContainer = "donkey-server"
	}
	if opts.GatewayContainer == "" {
		opts.GatewayContainer = "donkey-gateway"
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

// --- Naming ---------------------------------------------------------------

// netName is a space's network: the apps of one space share it and resolve
// each other by name.
func netName(space string) string { return "donkey-" + space }

// ctrName is one container's name. The dots keep donkey's own containers
// (donkey-server, donkey-gateway, …) out of the namespace.
func ctrName(space, name string) string { return "donkey." + space + "." + name }

// volName is an app's (or service's) data volume.
func volName(space, name string) string { return ctrName(space, name) + ".data" }

func appLabels(space, app, component string) map[string]string {
	return map[string]string{
		labelManaged:   "true",
		labelSpace:     space,
		labelApp:       app,
		labelComponent: component,
	}
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

// --- Spaces ---------------------------------------------------------------

func (r *Runtime) ValidateSpace(space string) error {
	if !config.ValidName(space) {
		return fmt.Errorf("%q is not a valid space name — lowercase letters, digits and dashes (max 40 chars)", space)
	}
	// The "system" space would collide with donkey's own records.
	if space == "system" {
		return fmt.Errorf("the space name %q is reserved for donkey itself — pick another (space: <name> in donkey.yaml)", space)
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
	if nw.Labels[labelManaged] != "true" {
		return fmt.Errorf("the network %s exists but isn't donkey's — pick a different space name", netName(space))
	}
	return nil
}

// ensureSpace creates the space's network and attaches donkey's own
// containers to it: the gateway to proxy, the server to probe and tunnel.
func (r *Runtime) ensureSpace(ctx context.Context, space string) error {
	if err := r.CheckSpaceFree(ctx, space); err != nil {
		return err
	}
	name := netName(space)
	_, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		_, err = r.cli.NetworkCreate(ctx, name, network.CreateOptions{
			Labels: map[string]string{labelManaged: "true", labelSpace: space},
		})
	}
	if err != nil {
		return err
	}
	for _, own := range []string{r.opts.ServerContainer, r.opts.GatewayContainer} {
		err := r.cli.NetworkConnect(ctx, name, own, nil)
		// Already attached is fine; a missing own container (local
		// development outside containers) must not fail the space.
		if err != nil && !strings.Contains(err.Error(), "already exists") && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Runtime) ListSpaces(ctx context.Context) ([]string, error) {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelManaged+"=true")),
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, nw := range nets {
		if space := nw.Labels[labelSpace]; space != "" {
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
	apps, err := r.spaceContainers(ctx, space, componentWeb)
	if err != nil {
		return err
	}
	if n := len(apps); n > 0 {
		word := "app"
		if n != 1 {
			word += "s"
		}
		return fmt.Errorf("the space still holds %d %s — remove them first", n, word)
	}
	// Services (and their data) go with the space.
	services, err := r.spaceContainers(ctx, space, "")
	if err != nil {
		return err
	}
	for _, c := range services {
		if err := r.removeContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	vols, err := r.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelSpace+"="+space)),
	})
	if err == nil {
		for _, v := range vols.Volumes {
			_ = r.cli.VolumeRemove(ctx, v.Name, false)
		}
	}
	if err := r.state.cleanupSpace(space); err != nil {
		return err
	}
	// donkey's own containers were attached to the network (to proxy and to
	// probe) — it only goes once they let it.
	for _, own := range []string{r.opts.ServerContainer, r.opts.GatewayContainer} {
		_ = r.cli.NetworkDisconnect(ctx, netName(space), own, true)
	}
	err = r.cli.NetworkRemove(ctx, netName(space))
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// spaceContainers lists a space's containers, all or one component.
func (r *Runtime) spaceContainers(ctx context.Context, space, component string) ([]container.Summary, error) {
	args := filters.NewArgs(
		filters.Arg("label", labelManaged+"=true"),
		filters.Arg("label", labelSpace+"="+space),
	)
	if component != "" {
		args.Add("label", labelComponent+"="+component)
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
			filters.Arg("label", labelComponent+"="+componentWeb),
		),
	})
	if err != nil {
		return nil, err
	}
	var out []rt.AppRef
	for _, c := range list {
		out = append(out, rt.AppRef{Space: c.Labels[labelSpace], App: c.Labels[labelApp]})
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
	st.Found = true
	st.UpToDate = true
	st.WantReplica = 1
	st.Image = info.Config.Labels[labelImage]
	if st.Image == "" {
		st.Image = info.Config.Image
	}
	if p, err := strconv.Atoi(info.Config.Labels[labelPort]); err == nil {
		st.Port = int32(p)
	}
	st.Restarts = int32(info.RestartCount)
	if info.State != nil {
		if info.State.Running {
			st.ReadyReplica = 1
		}
		if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil && t.Unix() > 0 {
			st.StartedAt = t
		}
		st.Instances = append(st.Instances, containerSummary(info))
	}
	if route, err := r.state.route(space, app); err == nil && route != nil {
		st.IngressHost = route.Host
		st.Protected = route.Login
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
	_, err := r.cli.VolumeInspect(ctx, volName(space, app))
	return err == nil
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
	// A space's services belong to the pseudo-app "shared"; their container
	// carries the service's own name.
	if svc, ok := strings.CutPrefix(opts.Component, "addon-"); ok {
		name = ctrName(space, svc)
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
	return r.state.binding(space, owner, name)
}

func (r *Runtime) aiProxyURL() string {
	return fmt.Sprintf("http://%s:8088/ai/v1", r.opts.ServerContainer)
}

func (r *Runtime) EnsureService(ctx context.Context, space string, spec rt.ServiceSpec) (map[string]string, string, string, error) {
	addon, err := addons.Get(spec.Type)
	if err != nil {
		return nil, "", "", rt.UserError{Err: err}
	}
	if !config.ValidName(spec.Name) {
		return nil, "", "", rt.UserError{Err: fmt.Errorf("%q is not a valid service name — lowercase letters, digits and dashes", spec.Name)}
	}
	if err := r.ensureSpace(ctx, space); err != nil {
		return nil, "", "", err
	}
	var binding map[string]string
	var version, storage string
	if spec.External {
		binding, err = addon.ExternalBinding(spec.URL)
		if err != nil {
			return nil, "", "", rt.UserError{Err: err}
		}
		if err := r.state.setBinding(space, sharedOwner, spec.Name, binding); err != nil {
			return nil, "", "", err
		}
	} else {
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// already holds.
		if info, err := r.cli.ContainerInspect(ctx, ctrName(space, spec.Name)); err == nil {
			if info.Config.Labels[labelComponent] == componentWeb {
				return nil, "", "", rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
			}
		}
		in := addons.Input{
			App: sharedOwner, Space: space, Namespace: netName(space),
			Name: spec.Name, Version: spec.Version, Storage: spec.Storage,
			AIProxyURL: r.aiProxyURL(),
		}
		if in.Version == "" {
			in.Version = addon.DefaultVersion()
		}
		if in.Storage == "" {
			in.Storage = addon.DefaultStorage()
		}
		existing, err := r.state.binding(space, sharedOwner, spec.Name)
		if err != nil {
			return nil, "", "", err
		}
		// Reuse the credential the instance was initialized with — rotating
		// it here would lock the service out of its own kept data.
		if pw, ok := existing[addon.CredentialKey()]; ok {
			in.Password = pw
		} else if in.Password, err = addons.NewPassword(); err != nil {
			return nil, "", "", err
		}
		binding = addon.Binding(in)
		// Kubernetes bindings speak in Service DNS (name.namespace.svc); on
		// Docker the plain container name is the address on the space
		// network, so the host part is rewritten.
		binding = rewriteBindingHosts(binding, in.Name, in.Namespace)
		if err := r.state.setBinding(space, sharedOwner, spec.Name, binding); err != nil {
			return nil, "", "", err
		}
		workload, err := addon.Workload(in)
		if err != nil {
			return nil, "", "", err
		}
		if workload != nil {
			if err := r.runService(ctx, space, spec.Name, in, workload, binding); err != nil {
				return nil, "", "", err
			}
		}
		version, storage = in.Version, in.Storage
	}
	// Attached apps hold copies; refresh them (and restart on external
	// changes, whose addresses may have moved).
	attached, err := r.state.attachments(space, spec.Name)
	if err != nil {
		return nil, "", "", err
	}
	for _, app := range attached {
		if err := r.state.setBinding(space, app, spec.Name, binding); err != nil {
			return nil, "", "", err
		}
		if spec.External {
			if _, err := r.Restart(ctx, space, app); err != nil {
				return nil, "", "", err
			}
		}
	}
	return binding, version, storage, nil
}

// rewriteBindingHosts swaps the Kubernetes Service address an addon binding
// speaks (name.namespace.svc) for the container name Docker's DNS answers.
func rewriteBindingHosts(binding map[string]string, name, namespace string) map[string]string {
	k8sHost := fmt.Sprintf("%s.%s.svc", name, namespace)
	out := map[string]string{}
	for k, v := range binding {
		out[k] = strings.ReplaceAll(v, k8sHost, name)
	}
	return out
}

// runService materializes an addon workload as a container with its data
// volume, and waits until it runs.
func (r *Runtime) runService(ctx context.Context, space, name string, in addons.Input, w *addons.Workload, binding map[string]string) error {
	labels := appLabels(space, sharedOwner, addons.Component(name))
	labels[labelPort] = strconv.Itoa(int(w.Port))

	vol := volName(space, name)
	if _, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: vol, Labels: labels}); err != nil {
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
		Image:   w.Image,
		Env:     env,
		Labels:  labels,
		Network: netName(space),
		Alias:   name,
		Volumes: map[string]volumeMount{w.DataPath: {Volume: vol}},
	}); err != nil {
		return err
	}
	return r.waitRunning(ctx, ctrName(space, name), 120*time.Second)
}

func (r *Runtime) AttachService(ctx context.Context, space, app, name string, binding map[string]string) (bool, error) {
	if err := r.state.setBinding(space, app, name, binding); err != nil {
		return false, err
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	return r.Restart(ctx, space, app)
}

func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	if err := r.state.removeBinding(space, app, name); err != nil {
		return err
	}
	_, err := r.Restart(ctx, space, app)
	return err
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData bool) ([]string, error) {
	attached, err := r.state.attachments(space, name)
	if err != nil {
		return nil, err
	}
	for _, app := range attached {
		if err := r.DetachService(ctx, space, app, name); err != nil {
			return nil, err
		}
	}
	if err := r.removeContainer(ctx, ctrName(space, name)); err != nil {
		return nil, err
	}
	if !keepData {
		if err := r.state.removeBinding(space, sharedOwner, name); err != nil {
			return nil, err
		}
		if err := r.cli.VolumeRemove(ctx, volName(space, name), false); err != nil && !cerrdefs.IsNotFound(err) {
			return nil, err
		}
	}
	return attached, nil
}

func (r *Runtime) ServiceAttachments(ctx context.Context, space, name string) ([]string, error) {
	return r.state.attachments(space, name)
}

func (r *Runtime) ServiceEndpoint(ctx context.Context, space, name string) (string, int32, error) {
	info, err := r.cli.ContainerInspect(ctx, ctrName(space, name))
	if cerrdefs.IsNotFound(err) {
		return "", 0, nil
	}
	if err != nil {
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
		info := rt.ServiceBindingInfo{Name: name}
		for k := range data {
			info.Keys = append(info.Keys, k)
		}
		sort.Strings(info.Keys)
		out = append(out, info)
	}
	return out, nil
}

func (r *Runtime) SharedServiceValues(ctx context.Context, key string) ([]string, error) {
	return r.state.bindingValues(key)
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
	} else if info.State == nil || !info.State.Running {
		gw.Detail = "not running"
	} else {
		gw.OK = true
		gw.Detail = "routing"
	}
	checks = append(checks, gw)

	return checks
}

// waitRunning polls until the container runs (steadily) or fails, returning
// a RolloutError with evidence.
func (r *Runtime) waitRunning(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stableSince := time.Time{}
	for {
		info, err := r.cli.ContainerInspect(ctx, name)
		if err != nil {
			return err
		}
		s := info.State
		app := info.Config.Labels[labelApp]
		switch {
		case s != nil && s.Running && !s.Restarting:
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			// A crash-looper is "running" between crashes; a short stable
			// window filters it out.
			if time.Since(stableSince) >= 3*time.Second {
				return nil
			}
		case s != nil && (s.Status == "exited" || s.Restarting || s.OOMKilled):
			return &rt.RolloutError{
				App:    app,
				Reason: containerSummary(info),
				Evidence: rt.Evidence{
					Summaries: []string{containerSummary(info)},
					Logs:      r.logsTail(ctx, name, 100),
				},
			}
		default:
			stableSince = time.Time{}
		}
		if time.Now().After(deadline) {
			return &rt.RolloutError{
				App:    app,
				Reason: "it didn't become healthy in time",
				Evidence: rt.Evidence{
					Summaries: []string{containerSummary(info)},
					Logs:      r.logsTail(ctx, name, 100),
				},
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
