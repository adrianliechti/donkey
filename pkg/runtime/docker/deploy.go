package docker

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// runSpec is everything needed to (re)create an app's container: stored at
// deploy, replayed by Restart — which is how changed env and bindings take
// effect.
type runSpec struct {
	// Image is the reference as the Docker daemon pulls it.
	Image       string   `json:"image"`
	Port        int      `json:"port"`
	Paths       []string `json:"paths,omitempty"` // storage paths, each a folder of the one volume
	Command     []string `json:"command,omitempty"`
	Healthcheck string   `json:"healthcheck,omitempty"`
	Host        string   `json:"host,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	CORS        string   `json:"cors,omitempty"`
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
// on Kubernetes — storage, declared services, the app, its routes — with
// Docker's vocabulary underneath.
func (r *Runtime) Deploy(ctx context.Context, spec rt.DeploySpec, out rt.Progress) (deployErr error) {
	app := spec.App
	space := spec.Space
	if err := config.ValidateAppName(app.Name); err != nil {
		return err
	}

	out.Line("Setting up on Docker…")
	if err := r.ensureSpace(ctx, space); err != nil {
		return err
	}
	if err := rt.CheckAppDomain(ctx, r, app, space, r.opts.Domain); err != nil {
		return err
	}
	recovery, err := r.prepareRecovery(ctx, space, app)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := recoveryContext(ctx)
		defer cancel()
		if deployErr != nil {
			if err := recovery.restore(cleanup); err != nil {
				// Recovery failure must stop automatic retries, which could
				// otherwise replace the only remaining copy of the original.
				deployErr = fmt.Errorf("%v; restoring the previous app failed: %v", deployErr, err)
			} else if recovery != nil && recovery.parked {
				out.Line("· Restored the previous app container and configuration.")
			}
		} else if err := recovery.discard(cleanup); err != nil {
			out.Line("· The app is ready; its stopped previous container could not be removed: %v", err)
		}
	}()

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
		if err := rt.CheckServiceAttachment(ctx, r, r.opts.CommonOfferings, space, app.Name, a.Name); err != nil {
			return err
		}
		shared, err := r.state.binding(space, sharedOwner, a.Name)
		if err != nil {
			return err
		}
		if len(shared) == 0 {
			if a.External {
				return fmt.Errorf("addon %s runs elsewhere and its address isn't stored in donkey.yaml — connect it once with `donkey addon attach %s --url …`", a.Name, a.Type)
			}
			out.Line("Setting up %s (%s) for the space — the first time can take a minute…", a.Name, a.Type)
			if shared, _, err = r.EnsureService(ctx, space, rt.ServiceSpec{Type: a.Type, Name: a.Name, Parameters: a.ServiceParameters()}); err != nil {
				return err
			}
			out.Line("✓ %s is ready.", a.Name)
		}
		if err := addons.BindEnvironment(app, a.Name, shared); err != nil {
			return err
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

	if host := r.appHost(space, app); host != "" {
		if owner := r.hostOwner(space, host, app.Name); owner != "" {
			return fmt.Errorf("%s already answers on %s — two apps cannot share one address (give one of them a different `host`)", owner, host)
		}
	}

	run := runSpec{Image: r.pullRef(spec.ImageRef), Port: app.Port, Command: app.Command,
		Healthcheck: app.Healthcheck.Path, Host: r.appHost(space, app), Mode: app.GatewayMode(), CORS: app.CORS}
	if app.HasStorage() {
		run.Paths = app.Storage.Paths
	}
	if err := r.setRunRecord(space, app.Name, run); err != nil {
		return err
	}

	if err := r.pullImage(ctx, run.Image); err != nil {
		return err
	}
	if err := recovery.park(ctx); err != nil {
		return err
	}

	if err := r.runApp(ctx, space, app.Name, run); err != nil {
		return err
	}
	if !spec.NoWait {
		out.Line("Waiting for the app to answer…")
		if err := r.waitRunning(ctx, ctrName(space, app.Name), 4*time.Minute); err != nil {
			return err
		}
	}

	// The routing table: the app's own hostname, and the paths it answers
	// on other apps' addresses.
	if err := r.applyRoutes(ctx, space, app, run.Port, spec.Hosts); err != nil {
		return err
	}
	for _, route := range app.Routes {
		out.Line("· Also answering https://%s.%s.%s%s", route.App, space, r.opts.Domain, route.Path)
	}
	if app.Internal {
		out.Line("· Internal only — the apps of this space reach it at http://%s", app.Name)
	} else if app.UsesOAuth() {
		out.Line("· API OAuth protection is in force.")
	} else if !app.Public {
		out.Line("· Login protection is in force.")
	}

	return nil
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (r *Runtime) InternalURL(space, app string, port int) string {
	return fmt.Sprintf("http://%s:%d", app, port)
}

func (r *Runtime) resolveEnv(space, app string, env map[string]string) error {
	for key, raw := range env {
		service, bindingKey, ref := config.ParseEnvRef(raw)
		if !ref {
			continue
		}
		binding, err := r.state.binding(space, app, service)
		if err != nil {
			return err
		}
		value, ok := binding[bindingKey]
		if !ok {
			return fmt.Errorf("service %s has no binding key %s for %s", service, bindingKey, key)
		}
		env[key] = value
	}
	return nil
}

// appHost is the address of an app — see config.AppHost for the rule.
func (r *Runtime) appHost(space string, app *config.AppConfig) string {
	return config.AppHost(app, space, r.opts.Domain)
}

// applyRoutes writes the app's routing table entries and retracts the ones
// it no longer claims.
func (r *Runtime) applyRoutes(ctx context.Context, space string, app *config.AppConfig, port int, hosts ...map[string]string) error {
	var settings config.SpaceConfig
	if _, err := r.state.readJSON(&settings, "spaces", space, "config.json"); err != nil {
		return err
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := settings.CheckAppHost(app, space, r.opts.Domain); err != nil {
		return rt.UserError{Err: err}
	}
	for _, target := range app.Routes {
		host := r.targetHost(space, target.App)
		if len(hosts) > 0 && hosts[0][target.App] != "" {
			host = hosts[0][target.App]
		}
		if err := settings.CheckHost(host); err != nil {
			return rt.UserError{Err: err}
		}
	}
	backend := fmt.Sprintf("http://%s:%d", ctrName(space, app.Name), port)
	login := !app.Public && !(app.Internal && app.IsAPI())

	host := r.appHost(space, app)
	if host == "" {
		if err := r.state.removeRoute(space, app.Name); err != nil {
			return err
		}
	} else {
		route := Route{Space: space, App: app.Name, Host: host, Path: "/", Backend: backend, Login: login, Mode: app.GatewayMode(), CORS: app.CORS}
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
			Space:       space,
			App:         app.Name,
			Host:        r.targetHost(space, target.App),
			Path:        target.Path,
			StripPrefix: target.StripPrefix,
			Backend:     backend,
			Login:       login,
			Mode:        app.GatewayMode(),
			CORS:        app.CORS,
		}
		if len(hosts) > 0 && hosts[0][target.App] != "" {
			route.Host = hosts[0][target.App]
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

// targetHost is the address another app of the space really answers on —
// it may sit at the space's own address, under a label of its own, or on a
// custom hostname. One that isn't deployed yet gets the default shape,
// which is what it will have when it is.
func (r *Runtime) targetHost(space, app string) string {
	if route, err := r.state.route(space, app); err == nil && route != nil && route.Host != "" {
		return route.Host
	}
	return app + "." + space + "." + r.opts.Domain
}

// hostOwner names the app already serving host in this space, or "" when it
// is free. Two apps on one hostname would both write a route for it and the
// gateway would serve whichever it sorted first — better to refuse than to
// let a deploy quietly take an address over.
func (r *Runtime) hostOwner(space, host, self string) string {
	names, err := r.state.listRoutes(space)
	if err != nil {
		return ""
	}
	for _, name := range names {
		if name == self || strings.HasPrefix(name, self+".on.") {
			continue
		}
		route, err := r.state.route(space, name)
		if err == nil && route != nil && route.Host == host && route.Path == "/" {
			// Path routes share a hostname on purpose; only an app's own
			// address (path "/") owns it.
			return strings.SplitN(name, ".on.", 2)[0]
		}
	}
	return ""
}

// pullImage fetches the image unless the daemon already has it.
func (r *Runtime) pullImage(ctx context.Context, ref string) error {
	opts, err := r.imagePullOptions(ref)
	if err != nil {
		return err
	}
	rc, err := r.cli.ImagePull(ctx, ref, opts)
	if err != nil {
		// The image may only exist locally (built and loaded, never pushed).
		if _, ierr := r.cli.ImageInspect(ctx, ref); ierr == nil {
			return nil
		}
		return fmt.Errorf("couldn't pull %s: %w", ref, err)
	}
	defer rc.Close()
	// The body has to be read, not discarded: the engine reports a pull that
	// failed partway (a rate limit, a layer that would not download) as a JSON
	// object inside the stream, with no transport error. Discarding it made a
	// failed pull look successful — and the caller removes the running
	// container before creating the new one.
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if derr := dec.Decode(&msg); derr != nil {
			if errors.Is(derr, io.EOF) {
				return nil
			}
			return fmt.Errorf("couldn't pull %s: %w", ref, derr)
		}
		if detail := cmp.Or(msg.ErrorDetail.Message, msg.Error); detail != "" {
			return fmt.Errorf("couldn't pull %s: %s", ref, detail)
		}
	}
}

// imagePullOptions authenticates runtime pulls with the image host's own
// credentials, including a BusyBox mirror on a separate registry.
func (r *Runtime) imagePullOptions(ref string) (image.PullOptions, error) {
	var opts image.PullOptions
	host, username, password, err := r.opts.Registry.ImageCredentials(ref)
	if err != nil {
		return opts, err
	}
	if username == "" || password == "" {
		return opts, nil
	}
	raw, err := json.Marshal(map[string]string{
		"username":      username,
		"password":      password,
		"serveraddress": host,
	})
	if err != nil {
		return opts, err
	}
	opts.RegistryAuth = base64.URLEncoding.EncodeToString(raw)
	return opts, nil
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
			if _, _, ref := config.ParseEnvRef(env[k]); ref {
				continue
			}
			env[k] = v
		}
	}
	if len(r.opts.TelemetryKey) > 0 {
		defaults := map[string]string{
			"OTEL_SERVICE_NAME":             app,
			"OTEL_RESOURCE_ATTRIBUTES":      "service.namespace=" + space,
			"OTEL_EXPORTER_OTLP_ENDPOINT":   "http://" + r.opts.TelemetryContainer + ":4318",
			"OTEL_EXPORTER_OTLP_PROTOCOL":   "http/protobuf",
			"OTEL_EXPORTER_OTLP_HEADERS":    "x-donkey-telemetry-token=" + telemetry.AppToken(r.opts.TelemetryKey, space, app),
			"OTEL_METRICS_EXPORTER":         "otlp",
			"OTEL_TRACES_EXPORTER":          "none",
			"OTEL_LOGS_EXPORTER":            "none",
			"OTEL_SEMCONV_STABILITY_OPT_IN": "http",
		}
		for key, value := range defaults {
			if _, configured := env[key]; !configured {
				env[key] = value
			}
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
	labels := appLabels(space, app, roleStorage)
	if err := r.ensureOwnedVolume(ctx, vol, labels); err != nil {
		return nil, err
	}
	volumes := map[string]volumeMount{}
	subs := make([]string, 0, len(spec.Paths))
	for _, p := range spec.Paths {
		sub := config.StorageSubPath(p)
		volumes[p] = volumeMount{Volume: vol, Subpath: sub}
		subs = append(subs, sub)
	}
	if err := r.initVolumeDirs(ctx, vol, subs, appLabels(space, app, roleInit)); err != nil {
		return nil, err
	}
	return volumes, nil
}

// initVolumeDirs creates (and opens up) the volume's folders in a one-shot
// helper. World-writable is deliberate: like the Kubernetes fsGroup, it is
// what lets an image running as any user write to its own disk.
func (r *Runtime) initVolumeDirs(ctx context.Context, vol string, subs []string, labels map[string]string) error {
	image := r.filesImage()
	if err := r.pullImage(ctx, image); err != nil {
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
	if err := r.removeOwnedContainer(ctx, name, labels); err != nil {
		return err
	}
	if _, err := r.cli.ContainerCreate(ctx, &container.Config{
		Image:      image,
		Entrypoint: args,
		Labels:     labels,
	}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: filesMount}},
	}, nil, nil, name); err != nil {
		return err
	}
	defer func() { _ = r.removeOwnedContainer(context.WithoutCancel(ctx), name, labels) }()
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
	if err := r.resolveEnv(space, app, env); err != nil {
		return err
	}
	labels := appLabels(space, app, roleWorkload)
	labels[labelImage] = spec.Image
	labels[labelPort] = strconv.Itoa(spec.Port)
	labels[labelHealthPath] = spec.Healthcheck
	labels[labelHost] = spec.Host
	labels[labelMode] = spec.Mode
	labels[labelCORS] = spec.CORS

	volumes, err := r.appMounts(ctx, space, app, spec)
	if err != nil {
		return err
	}
	return r.replaceContainer(ctx, ctrName(space, app), runConfig{
		Image:      spec.Image,
		Env:        env,
		Labels:     labels,
		Network:    netName(space),
		Alias:      app,
		Volumes:    volumes,
		Entrypoint: spec.Command,
	})
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
	if err := r.removeOwnedContainer(ctx, name, cfg.Labels); err != nil {
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
	if cfg.Labels[labelRole] == roleWorkload {
		hostCfg.SecurityOpt = []string{"no-new-privileges:true"}
		hostCfg.CapDrop = []string{"NET_RAW"}
	}
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
	for _, helper := range []struct{ name, role string }{
		{ctrName(space, app), roleWorkload},
		{filesHelperName(space, app), roleFiles},
		{volName(space, app) + ".init", roleInit},
	} {
		if err := r.removeOwnedContainer(ctx, helper.name,
			appLabels(space, app, helper.role)); err != nil {
			return err
		}
	}
	if !keepData {
		if err := r.removeOwnedVolume(ctx, volName(space, app), appLabels(space, app, roleStorage)); err != nil {
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
		st.Mode = route.Mode
		st.CORS = route.CORS
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
			Name: name, Host: route.Host, Path: route.Path, Protected: route.Login, Mode: route.Mode, CORS: route.CORS, StripPrefix: route.StripPrefix,
		})
	}
	return st, nil
}

// ApplyNetwork changes how the app is reached — without a deploy: the route
// files are rewritten, the gateway picks them up within seconds. Existing
// shared-address routes keep serving; their exposure follows the app's.
func (r *Runtime) ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (string, error) {
	space, app := cfg.Space, cfg.Name
	if err := rt.CheckAppDomain(ctx, r, cfg, space, r.opts.Domain); err != nil {
		return "", err
	}
	network, err := r.Network(ctx, space, app)
	if err != nil {
		return "", err
	}
	var hosts []string
	for _, route := range network.Routes {
		hosts = append(hosts, route.Host)
	}
	if err := rt.CheckSpaceDomains(ctx, r, space, hosts...); err != nil {
		return "", err
	}
	spec, err := r.runRecord(space, app)
	if err != nil {
		return "", err
	}
	port := cfg.Port
	if spec != nil {
		port = spec.Port
	}
	login := !cfg.Public && !(cfg.Internal && cfg.IsAPI())

	if host := r.appHost(space, cfg); host == "" {
		if err := r.state.removeRoute(space, app); err != nil {
			return "", err
		}
	} else {
		// The same guard a deploy applies: without it the console could hand
		// this app an address another one already serves, and the gateway
		// would answer with whichever it sorted first.
		if owner := r.hostOwner(space, host, app); owner != "" {
			return "", rt.UserError{Err: fmt.Errorf("%s already answers on %s — two apps cannot share one address", owner, host)}
		}
		route := Route{
			Space:   space,
			App:     app,
			Host:    host,
			Path:    "/",
			Backend: fmt.Sprintf("http://%s:%d", ctrName(space, app), port),
			Login:   login,
			Mode:    cfg.GatewayMode(),
			CORS:    cfg.CORS,
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
		route.Mode = cfg.GatewayMode()
		route.CORS = cfg.CORS
		if !login {
			route.Access = nil
		} else if route.Access == nil {
			route.Access = cfg.Access
		}
		if err := r.state.setRoute(space, name, *route); err != nil {
			return "", err
		}
	}
	if spec != nil {
		spec.Host = r.appHost(space, cfg)
		spec.Mode = cfg.GatewayMode()
		spec.CORS = cfg.CORS
		if err := r.setRunRecord(space, app, *spec); err != nil {
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
