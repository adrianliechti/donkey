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
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// preDeployTimeout bounds a pre-deploy step, same as on Kubernetes.
const preDeployTimeout = 15 * time.Minute

// runSpec is everything needed to (re)create an app's container: stored at
// deploy, replayed by Restart — which is how changed env and bindings take
// effect.
type runSpec struct {
	// Image is the reference as the Docker daemon pulls it.
	Image string   `json:"image"`
	Port  int      `json:"port"`
	Paths []string `json:"paths,omitempty"` // storage paths, each a folder of the one volume
}

func (r *Runtime) runRecord(space, app string) (*runSpec, error) {
	var spec runSpec
	found, err := r.state.readJSON(&spec, "deploy", space, app+".json")
	if err != nil || !found {
		return nil, err
	}
	return &spec, nil
}

func (r *Runtime) setRunRecord(space, app string, spec runSpec) error {
	return r.state.writeJSON(spec, "deploy", space, app+".json")
}

// Deploy takes a built image live as a container. The same choreography as
// on Kubernetes — storage, declared services, pre-deploy, the app, its
// routes — with Docker's vocabulary underneath.
func (r *Runtime) Deploy(ctx context.Context, spec rt.DeploySpec, out rt.Progress) error {
	app := spec.App
	space := spec.Space

	out.Line("Setting up on Docker…")
	if err := r.ensureSpace(ctx, space); err != nil {
		return err
	}

	if !app.HasStorage() && r.HasData(ctx, space, app.Name) {
		out.Line("· A disk from an earlier setup still exists (with its data). This deploy doesn't mount it — add its paths back under `storage:` in donkey.yaml to use it, or `donkey destroy` removes it.")
	}
	if app.HasStorage() {
		out.Line("· One disk (%s) for %s", app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
	}

	// donkey.yaml is declarative about addons: services belong to the space,
	// created on the spot when missing; the app keeps a current copy of each
	// binding.
	for _, a := range app.Addons {
		shared, err := r.state.binding(space, sharedOwner, a.Name)
		if err != nil {
			return err
		}
		if len(shared) == 0 {
			if a.External {
				return fmt.Errorf("addon %s runs elsewhere and its address isn't stored in donkey.yaml — connect it once with `donkey addon attach %s --url …`", a.Name, a.Type)
			}
			out.Line("Setting up %s (%s) for the space — the first time can take a minute…", a.Name, a.Type)
			if shared, _, _, err = r.EnsureService(ctx, space, rt.ServiceSpec{Type: a.Type, Name: a.Name, Version: a.Version, Storage: a.Storage}); err != nil {
				return err
			}
			out.Line("✓ %s is ready.", a.Name)
		}
		if err := r.state.setBinding(space, app.Name, a.Name, shared); err != nil {
			return err
		}
	}

	// donkey.yaml's own env lands in the stored env (its keys win), so the
	// running container and later restarts agree on what the app sees.
	if len(app.Env) > 0 {
		env, err := r.state.env(space, app.Name)
		if err != nil {
			return err
		}
		for k, v := range app.Env {
			env[k] = v
		}
		if err := r.state.setEnv(space, app.Name, env); err != nil {
			return err
		}
	}

	// What this deploy settled, for no-build deploys and the console.
	if err := r.SetAppMeta(ctx, space, app.Name, rt.MetaDecisions, optional(spec.Decisions)); err != nil {
		return err
	}

	run := runSpec{Image: r.pullRef(spec.ImageRef), Port: app.Port}
	if app.HasStorage() {
		run.Paths = app.Storage.Paths
	}
	if err := r.setRunRecord(space, app.Name, run); err != nil {
		return err
	}

	if err := r.pullImage(ctx, run.Image); err != nil {
		return err
	}

	if len(app.PreDeploy) > 0 {
		out.Line("Running the pre-deploy step…")
		if err := r.preDeploy(ctx, space, app.Name, run, app.PreDeploy); err != nil {
			// The app container was deliberately not touched, so whatever is
			// live stays live.
			return fmt.Errorf("the pre-deploy step failed, so nothing was changed and the running version keeps serving: %w", err)
		}
		out.Line("✓ Pre-deploy step finished.")
	}

	if err := r.runApp(ctx, space, app.Name, run); err != nil {
		return err
	}

	// The routing table: the app's own hostname, and the paths it answers
	// on other apps' addresses.
	if err := r.applyRoutes(ctx, space, app, run.Port); err != nil {
		return err
	}
	for _, route := range app.Routes {
		out.Line("· Also answering https://%s.%s.%s%s", route.App, space, r.opts.Domain, route.Path)
	}
	if app.Internal {
		out.Line("· Internal only — the apps of this space reach it at http://%s", app.Name)
	} else if !app.Public && r.opts.AuthAvailable {
		out.Line("· Login protection is in force.")
	}

	if spec.NoWait {
		return nil
	}
	out.Line("Waiting for the app to become healthy…")
	return r.waitRunning(ctx, ctrName(space, app.Name), 120*time.Second)
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// appHost is the address of an app: <app>.<space>.<domain>.
func (r *Runtime) appHost(space string, app *config.AppConfig) string {
	if app.Internal {
		return ""
	}
	if app.Domain != "" {
		return app.Domain
	}
	return app.Name + "." + space + "." + r.opts.Domain
}

// applyRoutes writes the app's routing table entries and retracts the ones
// it no longer claims.
func (r *Runtime) applyRoutes(ctx context.Context, space string, app *config.AppConfig, port int) error {
	backend := fmt.Sprintf("http://%s:%d", ctrName(space, app.Name), port)
	login := !app.Public && r.opts.AuthAvailable

	host := r.appHost(space, app)
	if host == "" {
		if err := r.state.removeRoute(space, app.Name); err != nil {
			return err
		}
	} else {
		route := Route{Host: host, Path: "/", Backend: backend, Login: login}
		if login && !app.Access.Empty() {
			route.Access = app.Access
		}
		if err := r.state.setRoute(space, app.Name, route); err != nil {
			return err
		}
	}

	keep := map[string]bool{}
	for _, target := range app.Routes {
		name := pathRouteName(app.Name, target.App, target.Path)
		keep[name] = true
		route := Route{
			Host:    target.App + "." + space + "." + r.opts.Domain,
			Path:    target.Path,
			Backend: backend,
			Login:   login,
		}
		if login && !app.Access.Empty() {
			route.Access = app.Access
		}
		if err := r.state.setRoute(space, name, route); err != nil {
			return err
		}
	}
	names, err := r.state.listRoutes(space)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, app.Name+".on.") && !keep[name] {
			if err := r.state.removeRoute(space, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// pullImage fetches the image unless the daemon already has it.
func (r *Runtime) pullImage(ctx context.Context, ref string) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		// The image may only exist locally (built and loaded, never pushed).
		if _, ierr := r.cli.ImageInspect(ctx, ref); ierr == nil {
			return nil
		}
		return fmt.Errorf("couldn't pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

// appEnv composes the container's environment: stored env, the addon
// bindings the app holds, and donkey's own PORT.
func (r *Runtime) appEnv(space, app string, port int) (map[string]string, error) {
	env, err := r.state.env(space, app)
	if err != nil {
		return nil, err
	}
	names, err := r.state.listBindings(space, app)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		data, err := r.state.binding(space, app, name)
		if err != nil {
			return nil, err
		}
		for k, v := range data {
			env[k] = v
		}
	}
	env["PORT"] = strconv.Itoa(port)
	return env, nil
}

// appMounts creates the app's data volume (with one folder per storage
// path — a Docker subpath mount refuses a folder that doesn't exist yet)
// and returns the mounts.
func (r *Runtime) appMounts(ctx context.Context, space, app string, spec runSpec) (map[string]volumeMount, error) {
	if len(spec.Paths) == 0 {
		return nil, nil
	}
	vol := volName(space, app)
	labels := appLabels(space, app, componentWeb)
	if _, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: vol, Labels: labels}); err != nil {
		return nil, err
	}
	volumes := map[string]volumeMount{}
	subs := make([]string, 0, len(spec.Paths))
	for _, p := range spec.Paths {
		sub := config.StorageSubPath(p)
		volumes[p] = volumeMount{Volume: vol, Subpath: sub}
		subs = append(subs, sub)
	}
	if err := r.initVolumeDirs(ctx, vol, subs); err != nil {
		return nil, err
	}
	return volumes, nil
}

// initVolumeDirs creates (and opens up) the volume's folders in a one-shot
// helper. World-writable is deliberate: like the Kubernetes fsGroup, it is
// what lets an image running as any user write to its own disk.
func (r *Runtime) initVolumeDirs(ctx context.Context, vol string, subs []string) error {
	if err := r.pullImage(ctx, filesImage); err != nil {
		return err
	}
	args := []string{"sh", "-c", ""}
	var paths []string
	for _, sub := range subs {
		paths = append(paths, filesMount+"/"+sub) // subs are [a-z0-9-] slugs
	}
	joined := strings.Join(paths, " ")
	args[2] = "mkdir -p " + joined + " && chmod 777 " + joined
	name := vol + ".init"
	if err := r.removeContainer(ctx, name); err != nil {
		return err
	}
	if _, err := r.cli.ContainerCreate(ctx, &container.Config{
		Image:      filesImage,
		Entrypoint: args,
	}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: filesMount}},
	}, nil, nil, name); err != nil {
		return err
	}
	defer func() { _ = r.removeContainer(context.WithoutCancel(ctx), name) }()
	if err := r.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return err
	}
	okC, errC := r.cli.ContainerWait(ctx, name, container.WaitConditionNotRunning)
	select {
	case status := <-okC:
		if status.StatusCode != 0 {
			return fmt.Errorf("preparing the disk's folders failed (exit code %d)", status.StatusCode)
		}
		return nil
	case err := <-errC:
		return err
	}
}

// runApp (re)creates the app's container from its run record.
func (r *Runtime) runApp(ctx context.Context, space, app string, spec runSpec) error {
	env, err := r.appEnv(space, app, spec.Port)
	if err != nil {
		return err
	}
	labels := appLabels(space, app, componentWeb)
	labels[labelImage] = spec.Image
	labels[labelPort] = strconv.Itoa(spec.Port)

	volumes, err := r.appMounts(ctx, space, app, spec)
	if err != nil {
		return err
	}
	return r.replaceContainer(ctx, ctrName(space, app), runConfig{
		Image:   spec.Image,
		Env:     env,
		Labels:  labels,
		Network: netName(space),
		Alias:   app,
		Volumes: volumes,
	})
}

// preDeploy runs the app's pre-deploy commands in a one-shot container with
// exactly the app's configuration — env, bindings, disk.
func (r *Runtime) preDeploy(ctx context.Context, space, app string, spec runSpec, steps []string) error {
	env, err := r.appEnv(space, app, spec.Port)
	if err != nil {
		return err
	}
	name := ctrName(space, app) + ".predeploy"
	volumes, err := r.appMounts(ctx, space, app, spec)
	if err != nil {
		return err
	}
	if err := r.replaceContainer(ctx, name, runConfig{
		Image:      spec.Image,
		Env:        env,
		Labels:     appLabels(space, app, "predeploy"),
		Network:    netName(space),
		Volumes:    volumes,
		Entrypoint: []string{"sh", "-c", strings.Join(steps, " && ")},
		OneShot:    true,
	}); err != nil {
		return err
	}
	defer func() { _ = r.removeContainer(context.WithoutCancel(ctx), name) }()

	waitCtx, cancel := context.WithTimeout(ctx, preDeployTimeout)
	defer cancel()
	okC, errC := r.cli.ContainerWait(waitCtx, name, container.WaitConditionNotRunning)
	select {
	case status := <-okC:
		if status.StatusCode != 0 {
			return &rt.JobError{
				Reason: fmt.Sprintf("it exited with code %d", status.StatusCode),
				Logs:   r.logsTail(ctx, name, 100),
			}
		}
		return nil
	case err := <-errC:
		if waitCtx.Err() != nil {
			return &rt.JobError{
				Reason: fmt.Sprintf("it was still running after %s", preDeployTimeout),
				Logs:   r.logsTail(ctx, name, 100),
			}
		}
		return err
	}
}

type volumeMount struct {
	Volume  string
	Subpath string
}

type runConfig struct {
	Image      string
	Env        map[string]string
	Labels     map[string]string
	Network    string
	Alias      string
	Volumes    map[string]volumeMount // target path -> volume
	Entrypoint []string               // override, for one-shot runs
	OneShot    bool                   // no restart policy, don't start-and-forget semantics change
}

// replaceContainer stops and removes whatever holds the name and creates
// (and starts) the new container — Docker's spelling of a rollout. The old
// container only goes once the image is present, so a failed pull never
// takes the running version down.
func (r *Runtime) replaceContainer(ctx context.Context, name string, cfg runConfig) error {
	if err := r.pullImage(ctx, cfg.Image); err != nil {
		return err
	}
	if err := r.removeContainer(ctx, name); err != nil {
		return err
	}

	env := make([]string, 0, len(cfg.Env))
	for _, k := range sortedEnvKeys(cfg.Env) {
		env = append(env, k+"="+cfg.Env[k])
	}
	var mounts []mount.Mount
	for target, vm := range cfg.Volumes {
		m := mount.Mount{Type: mount.TypeVolume, Source: vm.Volume, Target: target}
		if vm.Subpath != "" {
			m.VolumeOptions = &mount.VolumeOptions{Subpath: vm.Subpath}
		}
		mounts = append(mounts, m)
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })

	containerCfg := &container.Config{
		Image:  cfg.Image,
		Env:    env,
		Labels: cfg.Labels,
	}
	if cfg.Entrypoint != nil {
		containerCfg.Entrypoint = cfg.Entrypoint
		containerCfg.Cmd = nil
	}
	hostCfg := &container.HostConfig{Mounts: mounts}
	if !cfg.OneShot {
		hostCfg.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	}
	var netCfg *network.NetworkingConfig
	if cfg.Network != "" {
		endpoint := &network.EndpointSettings{}
		if cfg.Alias != "" {
			endpoint.Aliases = []string{cfg.Alias}
		}
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{cfg.Network: endpoint},
		}
	}
	if _, err := r.cli.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, name); err != nil {
		return err
	}
	return r.cli.ContainerStart(ctx, name, container.StartOptions{})
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Render says what Deploy would do — there is no YAML to show on Docker, so
// it is the plan in words.
func (r *Runtime) Render(spec rt.DeploySpec) (string, error) {
	app := spec.App
	var b strings.Builder
	fmt.Fprintf(&b, "container: %s\n", ctrName(spec.Space, app.Name))
	fmt.Fprintf(&b, "image:     %s\n", r.pullRef(spec.ImageRef))
	fmt.Fprintf(&b, "network:   %s (reachable by siblings as %q)\n", netName(spec.Space), app.Name)
	fmt.Fprintf(&b, "port:      %d\n", app.Port)
	if app.HasStorage() {
		fmt.Fprintf(&b, "volume:    %s (%s) mounted at %s\n", volName(spec.Space, app.Name), app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
	}
	if host := r.appHost(spec.Space, app); host != "" {
		exposure := "login-protected"
		if app.Public {
			exposure = "public"
		}
		fmt.Fprintf(&b, "route:     https://%s (%s)\n", host, exposure)
	} else {
		fmt.Fprintf(&b, "route:     none (internal)\n")
	}
	for _, target := range app.Routes {
		fmt.Fprintf(&b, "route:     https://%s.%s.%s%s\n", target.App, spec.Space, r.opts.Domain, target.Path)
	}
	for _, a := range app.Addons {
		fmt.Fprintf(&b, "service:   %s (%s)\n", a.Name, a.Type)
	}
	return b.String(), nil
}

func (r *Runtime) Destroy(ctx context.Context, space, app string, keepData bool) error {
	for _, name := range []string{
		ctrName(space, app),
		ctrName(space, app) + ".predeploy",
		ctrName(space, app) + ".files",
	} {
		if err := r.removeContainer(ctx, name); err != nil {
			return err
		}
	}
	if !keepData {
		if err := r.cli.VolumeRemove(ctx, volName(space, app), false); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	if err := r.state.cleanupApp(space, app, keepData); err != nil {
		return err
	}
	return r.state.remove("deploy", space, app+".json")
}

// --- Network --------------------------------------------------------------

func (r *Runtime) Network(ctx context.Context, space, app string) (*rt.NetworkState, error) {
	st := &rt.NetworkState{}
	if route, err := r.state.route(space, app); err != nil {
		return nil, err
	} else if route != nil {
		st.RouteFound = true
		st.Host = route.Host
		st.Protected = route.Login
		st.Access = route.Access
	}
	names, err := r.state.listRoutes(space)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, app+".on.") {
			continue
		}
		route, err := r.state.route(space, name)
		if err != nil || route == nil {
			continue
		}
		st.Routes = append(st.Routes, rt.PathRouteState{
			Name: name, Host: route.Host, Path: route.Path, Protected: route.Login,
		})
	}
	return st, nil
}

// ApplyNetwork changes how the app is reached — without a deploy: the route
// files are rewritten, the gateway picks them up within seconds. Existing
// shared-address routes keep serving; their exposure follows the app's.
func (r *Runtime) ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (string, error) {
	space, app := cfg.Space, cfg.Name
	spec, err := r.runRecord(space, app)
	if err != nil {
		return "", err
	}
	port := cfg.Port
	if spec != nil {
		port = spec.Port
	}
	login := !cfg.Public && r.opts.AuthAvailable

	if host := r.appHost(space, cfg); host == "" {
		if err := r.state.removeRoute(space, app); err != nil {
			return "", err
		}
	} else {
		route := Route{
			Host:    host,
			Path:    "/",
			Backend: fmt.Sprintf("http://%s:%d", ctrName(space, app), port),
			Login:   login,
		}
		if login && !cfg.Access.Empty() {
			route.Access = cfg.Access
		}
		if err := r.state.setRoute(space, app, route); err != nil {
			return "", err
		}
	}

	names, err := r.state.listRoutes(space)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, app+".on.") {
			continue
		}
		route, err := r.state.route(space, name)
		if err != nil || route == nil {
			continue
		}
		route.Login = login
		if !login {
			route.Access = nil
		} else if route.Access == nil {
			route.Access = cfg.Access
		}
		if err := r.state.setRoute(space, name, *route); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (r *Runtime) SetAccess(ctx context.Context, space, app string, access *config.AccessConfig) error {
	update := func(name string) error {
		route, err := r.state.route(space, name)
		if err != nil || route == nil || !route.Login {
			return err
		}
		route.Access = access
		return r.state.setRoute(space, name, *route)
	}
	if err := update(app); err != nil {
		return err
	}
	names, err := r.state.listRoutes(space)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, app+".on.") {
			if err := update(name); err != nil {
				return err
			}
		}
	}
	return nil
}
