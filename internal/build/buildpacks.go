package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"
)

// The Cloud Native Buildpacks builder runs the CNB lifecycle inside the
// builder image, in the build engine donkey already has — no `pack` binary,
// no new cluster component, nothing installed on anyone's machine.
//
// It is the path that works on a network whose only egress is a registry
// mirror: unlike railpack, nothing about the app is detected outside the
// build, and every toolchain is already in the builder image. The two images
// it needs (the builder and the run image) are ordinary references, so a
// mirrored installation points them at its mirror.
const (
	// The paths the CNB platform specification fixes.
	cnbAppDir      = "/workspace"
	cnbLayersDir   = "/layers"
	cnbPlatformDir = "/platform"
	cnbCacheDir    = "/cache"
	cnbReportName  = "report.toml"
	cnbReportPath  = cnbLayersDir + "/" + cnbReportName
	cnbCreator     = "/cnb/lifecycle/creator"

	// cnbBuilderMetadataLabel is where a builder image records the run image
	// it was built to pair with.
	cnbBuilderMetadataLabel = "io.buildpacks.builder.metadata"

	// cnbRegistryAuthEnv is how the lifecycle is given registry credentials:
	// a JSON object of host to Authorization header value.
	cnbRegistryAuthEnv = "CNB_REGISTRY_AUTH"

	// cnbRegistryCAPath is where donkey's registry CA is placed inside the
	// build. Go reads every file in /etc/ssl/certs on top of the system
	// bundle, so dropping one there adds a trusted root without rewriting
	// the image's own — and the CNB lifecycle is a Go program.
	cnbRegistryCAPath = "/etc/ssl/certs/donkey-registry-ca.crt"

	// The identity the lifecycle runs the build as when the builder image
	// says nothing else.
	cnbDefaultUID = 1000
	cnbDefaultGID = 1000
)

// buildBuildpacks runs the CNB lifecycle's creator, which detects, builds,
// and pushes the image in one pass.
//
// The lifecycle does the pushing, so this solve exports no image. What comes
// back instead is the lifecycle's own report, exported as a file: it names
// the digest that was actually pushed, which is the answer donkey needs and
// the only authoritative one.
func (b *BuildKitBuilder) buildBuildpacks(ctx context.Context, c *bkclient.Client, req Request) (*Result, error) {
	if !req.Push {
		return nil, fmt.Errorf("the buildpacks builder always publishes the image it builds — it cannot build without pushing")
	}
	auth, err := registryAuth(b.Registry)
	if err != nil {
		return nil, err
	}
	contextFS, err := fsutil.NewFS(req.ContextDir)
	if err != nil {
		return nil, err
	}
	platform, err := platforms.Parse(req.Platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", req.Platform, err)
	}

	buildEnv := b.Settings.MergeEnv(req.BuildEnv)

	// Everything the build is handed travels as a BuildKit secret: build
	// variables routinely carry the credentials of a private package
	// registry, and a secret is mounted for the one command that needs it
	// rather than baked into a cached layer.
	secrets := map[string][]byte{}
	for k, v := range buildEnv {
		secrets[cnbEnvSecretID(k)] = []byte(v)
	}
	registryAuthJSON, err := b.cnbRegistryAuth(buildEnv)
	if err != nil {
		return nil, err
	}
	secrets[cnbRegistryAuthEnv] = registryAuthJSON

	// The lifecycle pushes the image from inside the build, so it — not the
	// build engine — is what has to trust a registry whose certificate comes
	// from a private CA. buildkitd's own copy is no help there, so the
	// certificate travels into the build.
	var registryCA []byte
	if b.Registry.CAFile != "" {
		if registryCA, err = os.ReadFile(b.Registry.CAFile); err != nil {
			return nil, fmt.Errorf("reading the registry's certificate authority: %w", err)
		}
	}

	// The report is the one thing this build produces locally.
	reportDir, err := os.MkdirTemp("", "donkey-cnb-report-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(reportDir)

	opt := bkclient.SolveOpt{
		Exports: []bkclient.ExportEntry{{
			Type:      bkclient.ExporterLocal,
			OutputDir: reportDir,
		}},
		Session:     []session.Attachable{auth, secretsprovider.FromMap(secrets)},
		LocalMounts: map[string]fsutil.FS{"context": contextFS},
	}

	buildFn := func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		builderRef, err := NormalizeImageRef(b.Settings.BuildpacksBuilderImage())
		if err != nil {
			return nil, err
		}
		resolved, _, configBytes, err := gw.ResolveImageConfig(ctx, builderRef, sourceresolver.Opt{
			LogName:  "resolve buildpacks builder " + builderRef,
			ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &platform},
		})
		if err != nil {
			return nil, fmt.Errorf("the buildpacks builder image %s couldn't be read: %w", builderRef, err)
		}
		if resolved != "" {
			// Pinned to the digest that was just resolved, so the build and
			// the image it was planned against cannot drift apart.
			builderRef = resolved
		}
		builder, err := parseBuilderImage(configBytes)
		if err != nil {
			return nil, fmt.Errorf("the buildpacks builder image %s is not usable: %w", builderRef, err)
		}
		runImage := b.Settings.Buildpacks.RunImage
		if runImage == "" {
			runImage = builder.RunImage
		}
		if runImage == "" {
			return nil, fmt.Errorf(
				"the buildpacks builder image %s doesn't name a run image, so donkey doesn't know what to base the app on — set build.buildpacks.runImage", builderRef)
		}

		def, err := cnbDefinition(ctx, cnbBuild{
			BuilderRef:  builderRef,
			BuilderInfo: builder,
			RunImage:    runImage,
			ImageRef:    req.ImageRef,
			Platform:    platform,
			PlatformAPI: b.Settings.Buildpacks.PlatformAPI,
			BuildEnv:    buildEnv,
			Insecure:    b.Registry.Insecure,
			RegistryCA:  registryCA,
			CacheID:     cnbCacheID(req.ImageRef),
		})
		if err != nil {
			return nil, err
		}
		return gw.Solve(ctx, gateway.SolveRequest{Definition: def.ToPB()})
	}

	if err := b.withProgress(ctx, req, func(ctx context.Context, ch chan *bkclient.SolveStatus) error {
		_, err := c.Build(ctx, opt, "donkey", buildFn, ch)
		return err
	}); err != nil {
		return nil, err
	}

	digest, err := readCNBReport(filepath.Join(reportDir, cnbReportName))
	if err != nil {
		return nil, err
	}
	return &Result{
		ImageRef: req.ImageRef,
		Digest:   digest,
		LogPath:  req.Progress.LogPath,
	}, nil
}

// cnbBuild is everything the lifecycle invocation needs.
type cnbBuild struct {
	BuilderRef  string
	BuilderInfo builderImage
	RunImage    string
	ImageRef    string
	Platform    ocispecs.Platform
	PlatformAPI string
	BuildEnv    map[string]string
	Insecure    bool
	// RegistryCA is the certificate authority donkey's registry serves under,
	// when it is not a publicly trusted one.
	RegistryCA []byte
	CacheID    string
}

// cnbDefinition builds the graph: the builder image, the app copied in, and
// one command — the lifecycle's creator, which does everything else.
func cnbDefinition(ctx context.Context, in cnbBuild) (*llb.Definition, error) {
	uid, gid := in.BuilderInfo.UID, in.BuilderInfo.GID
	owner := llb.WithUIDGID(uid, gid)

	st := llb.Image(in.BuilderRef, llb.Platform(in.Platform))
	// A builder image ships /workspace and /layers already owned by the build
	// user, and nothing at all under /platform. All three are created anyway:
	// mkdir -p over a directory that exists changes nothing, and it is the
	// only way the ownership is donkey's to state rather than to assume.
	st = st.File(llb.Mkdir(cnbAppDir, 0o755, llb.WithParents(true), owner))
	st = st.File(llb.Mkdir(cnbLayersDir, 0o755, llb.WithParents(true), owner))
	st = st.File(llb.Mkdir(cnbPlatformDir+"/env", 0o755, llb.WithParents(true), owner))
	if len(in.RegistryCA) > 0 {
		st = st.File(llb.Mkdir(path.Dir(cnbRegistryCAPath), 0o755, llb.WithParents(true)))
		st = st.File(llb.Mkfile(cnbRegistryCAPath, 0o444, in.RegistryCA))
	}
	st = st.File(llb.Copy(llb.Local("context"), "/", cnbAppDir, &llb.CopyInfo{
		CopyDirContentsOnly: true,
		CreateDestPath:      true,
		// The lifecycle chowns the layer and cache directories itself, but
		// never the app: the buildpacks read (and write) it as the build
		// user, so it has to arrive owned by them.
		ChownOpt: &llb.ChownOpt{User: &llb.UserOpt{UID: uid}, Group: &llb.UserOpt{UID: gid}},
	}))

	// The builder image's own environment is applied by hand: raw LLB, unlike
	// a Dockerfile build, inherits nothing from the image it starts from, and
	// the lifecycle needs its PATH and its CNB_* settings.
	imageSetsPlatformAPI := false
	for _, kv := range in.BuilderInfo.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		st = st.AddEnv(k, v)
		if k == "CNB_PLATFORM_API" && v != "" {
			imageSetsPlatformAPI = true
		}
	}
	// The lifecycle exits immediately unless it is told which platform
	// contract to speak. Configuration wins, then whatever the builder image
	// already sets, then the newest version the builder says it supports.
	switch {
	case in.PlatformAPI != "":
		st = st.AddEnv("CNB_PLATFORM_API", in.PlatformAPI)
	case imageSetsPlatformAPI:
	case in.BuilderInfo.PlatformAPI != "":
		st = st.AddEnv("CNB_PLATFORM_API", in.BuilderInfo.PlatformAPI)
	default:
		return nil, fmt.Errorf(
			"the buildpacks builder image %s doesn't say which CNB platform version its lifecycle speaks, and it won't run without one — set build.buildpacks.platformApi", in.BuilderRef)
	}
	if in.Insecure {
		// donkey's registry speaks plain http, and the lifecycle — not
		// BuildKit — is what pushes to it.
		//
		// Only donkey's own registry is listed, because it is the only one
		// whose scheme donkey knows: an image reference carries no scheme, so
		// a run image on a plain-http mirror cannot be recognized as one. An
		// installation in that position sets CNB_INSECURE_REGISTRIES in
		// build.env, which replaces this outright.
		st = st.AddEnv("CNB_INSECURE_REGISTRIES", registryHostOf(in.ImageRef))
	}
	// Build variables are mounted as secrets, which by design do not
	// participate in the cache key. Their hash does, so changing one rebuilds
	// instead of returning the image built with the old values.
	if h := secretsHash(in.BuildEnv); h != "" {
		st = st.AddEnv("DONKEY_BUILD_ENV_HASH", h)
	}
	// The creator starts as root only to chown the layer and cache
	// directories to the build user, then drops to -uid/-gid itself. This is
	// what lets a cache volume the engine created be written by the build.
	st = st.User("root")

	args := []string{
		cnbCreator,
		"-app", cnbAppDir,
		"-layers", cnbLayersDir,
		"-platform", cnbPlatformDir,
		"-cache-dir", cnbCacheDir,
		"-report", cnbReportPath,
		"-run-image", in.RunImage,
		"-uid", strconv.Itoa(uid),
		"-gid", strconv.Itoa(gid),
		in.ImageRef,
	}
	runOpts := []llb.RunOption{
		llb.Args(args),
		llb.WithCustomNamef("[buildpacks] build and push %s", in.ImageRef),
		// One cache directory per app, and locked rather than shared: the
		// lifecycle's cache is not written to be safe against a second build
		// of the same app running beside it. Different apps have different
		// ids, so nothing serializes across them.
		llb.AddMount(cnbCacheDir, llb.Scratch(),
			llb.AsPersistentCacheDir(in.CacheID, llb.CacheMountLocked)),
		llb.AddSecret(cnbRegistryAuthEnv,
			llb.SecretID(cnbRegistryAuthEnv), llb.SecretAsEnv(true)),
	}
	// Build variables reach the build twice, because two different things
	// read them: the platform directory is the CNB contract for what
	// buildpacks see, and the process environment is what the lifecycle
	// itself reads — a forward proxy has to be there for it to pull the run
	// image at all.
	for _, k := range slices.Sorted(maps.Keys(in.BuildEnv)) {
		id := cnbEnvSecretID(k)
		runOpts = append(runOpts,
			llb.AddSecret(cnbPlatformDir+"/env/"+k,
				llb.SecretID(id), llb.SecretFileOpt(uid, gid, 0o444)),
			llb.AddSecret(k, llb.SecretID(id), llb.SecretAsEnv(true)),
		)
	}

	run := st.Run(runOpts...)
	// Nothing of the build's filesystem is wanted — only what the lifecycle
	// says it pushed.
	report := llb.Scratch().File(llb.Copy(run.Root(), cnbReportPath, "/"+cnbReportName))
	def, err := report.Marshal(ctx, llb.Platform(in.Platform))
	if err != nil {
		return nil, fmt.Errorf("preparing the build steps: %w", err)
	}
	return def, nil
}

// builderImage is what donkey reads out of a CNB builder image: who the build
// runs as, what the app should be based on, which platform contract its
// lifecycle speaks, and the environment it expects to start with.
type builderImage struct {
	Env      []string
	UID, GID int
	RunImage string
	// PlatformAPI is the newest CNB platform version this builder's lifecycle
	// supports. The lifecycle refuses to start without being told one, and
	// the answer differs per builder — so donkey asks the builder rather than
	// picking a number and hoping.
	PlatformAPI string
}

func parseBuilderImage(configBytes []byte) (builderImage, error) {
	var cfg ocispecs.Image
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return builderImage{}, fmt.Errorf("its configuration couldn't be read: %w", err)
	}
	out := builderImage{Env: cfg.Config.Env, UID: cnbDefaultUID, GID: cnbDefaultGID}
	// A builder image states the build user in its environment; the spec
	// requires it, and it is the only reliable place — USER may be a name
	// this side of the build cannot resolve.
	for _, kv := range cfg.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch k {
		case "CNB_USER_ID":
			out.UID = n
		case "CNB_GROUP_ID":
			out.GID = n
		}
	}
	metadata := cfg.Config.Labels[cnbBuilderMetadataLabel]
	out.RunImage = builderRunImage(metadata)
	out.PlatformAPI = builderPlatformAPI(metadata)
	return out, nil
}

// builderPlatformAPI reads the newest platform contract the builder's
// lifecycle accepts. The lifecycle will not start without CNB_PLATFORM_API,
// and there is no safe constant to use instead: too new and an older builder
// refuses it, too old and a newer one has dropped support. The builder says
// which versions it takes, so that is what donkey uses.
func builderPlatformAPI(metadata string) string {
	if metadata == "" {
		return ""
	}
	var m struct {
		Lifecycle struct {
			API struct {
				Platform string `json:"platform"`
			} `json:"api"`
			APIs struct {
				Platform struct {
					Supported []string `json:"supported"`
				} `json:"platform"`
			} `json:"apis"`
		} `json:"lifecycle"`
	}
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return ""
	}
	best := ""
	for _, v := range m.Lifecycle.APIs.Platform.Supported {
		if compareAPIVersions(v, best) > 0 {
			best = v
		}
	}
	if best != "" {
		return best
	}
	// Older builders list only the one version they were built against.
	return m.Lifecycle.API.Platform
}

// compareAPIVersions orders "major.minor" numerically — "0.9" is older than
// "0.15", which string order gets backwards.
func compareAPIVersions(a, b string) int {
	part := func(v string) (int, int) {
		major, minor, _ := strings.Cut(v, ".")
		m, _ := strconv.Atoi(major)
		n, _ := strconv.Atoi(minor)
		return m, n
	}
	if b == "" {
		if a == "" {
			return 0
		}
		return 1
	}
	if a == "" {
		return -1
	}
	amaj, amin := part(a)
	bmaj, bmin := part(b)
	switch {
	case amaj != bmaj:
		return amaj - bmaj
	default:
		return amin - bmin
	}
}

// builderRunImage reads the run image a builder was paired with. Two spellings
// are in the wild — the newer top-level list and the older stack block — and
// builders that have both agree, so either answer is the right one.
//
// Note what this address has to satisfy: the lifecycle pulls it from inside
// the build, so it is not covered by the build engine's registry mirrors. An
// installation behind a mirror sets build.buildpacks.runImage to the mirrored
// reference explicitly.
func builderRunImage(metadata string) string {
	if metadata == "" {
		return ""
	}
	var m struct {
		RunImages []struct {
			Image string `json:"image"`
		} `json:"runImages"`
		Stack struct {
			RunImage struct {
				Image string `json:"image"`
			} `json:"runImage"`
		} `json:"stack"`
	}
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return ""
	}
	for _, r := range m.RunImages {
		if r.Image != "" {
			return r.Image
		}
	}
	return m.Stack.RunImage.Image
}

// cnbRegistryAuth is how the lifecycle logs in to push the app image: a JSON
// object of registry host to Authorization header value. donkey assembles it
// from the credentials it already holds, so nothing is configured twice.
//
// A build variable of the same name wins outright — that is the way in for an
// installation whose mirror needs credentials donkey has no other reason to
// know.
func (b *BuildKitBuilder) cnbRegistryAuth(buildEnv map[string]string) ([]byte, error) {
	if v, ok := buildEnv[cnbRegistryAuthEnv]; ok {
		return []byte(v), nil
	}
	password, err := b.Registry.Password()
	if err != nil {
		return nil, err
	}
	auth := map[string]string{}
	if b.Registry.Username != "" || password != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(b.Registry.Username + ":" + password))
		auth[b.Registry.Host()] = "Basic " + credentials
	}
	return json.Marshal(auth)
}

// cnbEnvSecretID namespaces a build variable's secret so it cannot collide
// with the registry credentials travelling beside it.
func cnbEnvSecretID(key string) string { return "donkey-build-env:" + key }

// cnbCacheID keys the lifecycle's cache to the app, not to the deploy: the
// image tag carries a timestamp, so keying on it would mean an empty cache
// every time.
func cnbCacheID(imageRef string) string {
	repo := imageRef
	// Only the last path element can carry a tag — a registry's port colon is
	// not one.
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		if name, _, ok := strings.Cut(repo[i+1:], ":"); ok {
			repo = repo[:i+1] + name
		}
	}
	return "donkey-cnb-" + repo
}

// registryHostOf is the host an image reference lives on.
func registryHostOf(imageRef string) string {
	host, _, _ := strings.Cut(imageRef, "/")
	return host
}

// readCNBReport reads the digest the lifecycle says it pushed. This is the
// authoritative answer: with no image exporter in play, there is no
// containerimage.digest to read, and the report is written after the push
// succeeded.
func readCNBReport(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the buildpacks lifecycle didn't leave a report behind, so donkey can't tell what it pushed: %w", err)
	}
	var report struct {
		Image struct {
			Tags   []string `toml:"tags"`
			Digest string   `toml:"digest"`
		} `toml:"image"`
	}
	if err := toml.Unmarshal(data, &report); err != nil {
		return "", fmt.Errorf("the buildpacks lifecycle's report couldn't be read: %w", err)
	}
	if report.Image.Digest == "" {
		return "", fmt.Errorf("the buildpacks lifecycle's report names no image digest")
	}
	return report.Image.Digest, nil
}
