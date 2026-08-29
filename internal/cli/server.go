package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func serverCommand() *cli.Command {
	return &cli.Command{
		Name:  "server",
		Usage: "configure and deploy the donkey control plane",
		Commands: []*cli.Command{
			serverInitCommand(),
			serverRenderCommand(),
			serverInstallCommand(),
		},
	}
}

// serverInstallCommand is the one command that talks to Kubernetes directly
// (via kubeconfig): it bootstraps the control plane the CLI then talks to.
// The cluster (and its ingress controller) must already exist.
// Re-running with changed settings updates the installation (server-side apply).
func serverInstallCommand() *cli.Command {
	return &cli.Command{
		Name:   "install",
		Usage:  "install (or update) the donkey control plane into an existing cluster",
		Flags:  serverSettingsFlags(),
		Action: serverInstallAction,
	}
}

// serverSettingsFlags returns fresh flag values for every command which reads
// an install file. urfave keeps parsing state on a Flag, so sharing the slice
// itself between install and render would make one command affect the other.
func serverSettingsFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "settings file (see docs); flags override individual values"},
		&cli.StringFlag{Name: "runtime", Usage: "where donkey runs apps: kubernetes (default) or docker (straight on a Docker engine, no cluster)"},
		&cli.StringFlag{Name: "image", Usage: "donkey-server image reference (pullable by the cluster)"},
		&cli.StringFlag{Name: "buildkit-image", Usage: "build engine image (default: a digest-pinned moby/buildkit)"},
		&cli.StringFlag{Name: "registry-url", Usage: "registry prefix for app images, as reachable FROM the cluster"},
		&cli.StringFlag{Name: "registry-username", Usage: "registry login"},
		&cli.BoolFlag{Name: "registry-insecure", Usage: "registry is plain HTTP (local/testing)"},
		&cli.BoolFlag{Name: "registry-immutable", Usage: "registry refuses to overwrite tags (ECR/GHCR/Harbor immutability) — donkey then makes every tag unique"},
		&cli.StringFlag{Name: "registry-ca-secret", Usage: "secret in donkey-system holding ca.crt of the registry's certificate authority"},
		&cli.StringFlag{Name: "domain", Usage: "apps become <app>.<space>.<domain>"},
		&cli.StringFlag{Name: "server-host", Usage: "hostname of the API and console (default: the domain itself)"},
		&cli.StringFlag{Name: "gateway-service-type", Usage: "service type traffic enters through: LoadBalancer (default), ClusterIP or NodePort (clusters without a load-balancer controller, e.g. kind)"},
		&cli.StringFlag{Name: "default-platform", Usage: "build platform of the cluster nodes (default linux/amd64)"},
		&cli.BoolFlag{Name: "no-verify", Usage: "skip running a freshly built image to check it starts"},
		&cli.StringFlag{Name: "https-proxy", Usage: "forward proxy for donkey-server and the build engine"},
		&cli.StringFlag{Name: "no-proxy", Usage: "hosts that bypass the forward proxy, comma-separated"},
		&cli.StringFlag{Name: "tls-mode", Usage: "how the gateway gets certificates: acme (default), ca (built-in CA for local/internal installs) or none (behind a TLS-terminating cluster gateway)"},
		&cli.StringFlag{Name: "acme-email", Usage: "ACME account contact for expiry warnings (tls-mode acme)"},
		&cli.StringFlag{Name: "acme-directory", Usage: "ACME endpoint override, e.g. Let's Encrypt staging (tls-mode acme)"},
		&cli.StringFlag{Name: "oidc-issuer", Usage: "OIDC issuer for the login gateway (e.g. Entra ID); empty = no gateway"},
		&cli.StringFlag{Name: "oidc-client-id", Usage: "OIDC client id for the gateway"},
		&cli.StringFlag{Name: "oidc-client-secret", Usage: "OIDC client secret (or $DONKEY_OIDC_CLIENT_SECRET)"},
		&cli.StringFlag{Name: "openai-base-url", Usage: "OpenAI-compatible endpoint for AI assistance (as reachable FROM the cluster)"},
		&cli.StringFlag{Name: "openai-api-key", Usage: "API key for AI assistance (or $OPENAI_API_KEY)"},
		&cli.StringFlag{Name: "ai-model", Usage: "model for AI assistance"},
	}
}

// installSettings merges the settings file with flag overrides.
func installSettings(cmd *cli.Command) (*config.InstallFile, error) {
	return loadInstallSettings(cmd, true)
}

// loadInstallSettings can leave the process environment's secrets unresolved
// for offline rendering. Explicit file/flag values are still parsed so they
// can be validated, but the renderer replaces them before producing output.
func loadInstallSettings(cmd *cli.Command, resolveSecretEnv bool) (*config.InstallFile, error) {
	settings := &config.InstallFile{}
	if path := cmd.String("file"); path != "" {
		loaded, err := config.LoadInstallFile(path)
		if err != nil {
			return nil, err
		}
		settings = loaded
	}
	strOver := func(dst *string, flag string) {
		if v := cmd.String(flag); v != "" {
			*dst = v
		}
	}
	strOver(&settings.Runtime, "runtime")
	strOver(&settings.Image, "image")
	strOver(&settings.BuildkitImage, "buildkit-image")
	strOver(&settings.Registry.URL, "registry-url")
	strOver(&settings.Registry.Username, "registry-username")
	strOver(&settings.Registry.CASecret, "registry-ca-secret")
	strOver(&settings.Domain, "domain")
	strOver(&settings.ServerHost, "server-host")
	strOver(&settings.Gateway.ServiceType, "gateway-service-type")
	strOver(&settings.DefaultPlatform, "default-platform")
	if cmd.Bool("no-verify") {
		settings.Build.Verify = new(false)
	}
	strOver(&settings.Build.Proxy.HTTPS, "https-proxy")
	strOver(&settings.Build.Proxy.NoProxy, "no-proxy")
	strOver(&settings.Auth.Issuer, "oidc-issuer")
	strOver(&settings.Auth.ClientID, "oidc-client-id")
	strOver(&settings.Auth.ClientSecret, "oidc-client-secret")
	strOver(&settings.AI.BaseURL, "openai-base-url")
	strOver(&settings.AI.APIKey, "openai-api-key")
	strOver(&settings.AI.Model, "ai-model")
	strOver(&settings.TLS.Mode, "tls-mode")
	strOver(&settings.TLS.Email, "acme-email")
	strOver(&settings.TLS.Directory, "acme-directory")
	if cmd.Bool("registry-insecure") {
		settings.Registry.Insecure = true
	}
	if cmd.Bool("registry-immutable") {
		settings.Registry.Immutable = true
	}
	if !resolveSecretEnv {
		return settings, nil
	}
	// Secrets may come from env instead of file/flags for a live install.
	if settings.Auth.ClientSecret == "" {
		settings.Auth.ClientSecret = os.Getenv("DONKEY_OIDC_CLIENT_SECRET")
	}
	if settings.AI.APIKey == "" {
		settings.AI.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if settings.Build.Packages.Password == "" {
		settings.Build.Packages.Password = os.Getenv("DONKEY_PACKAGES_PASSWORD")
	}
	return settings, nil
}

func serverInstallAction(ctx context.Context, cmd *cli.Command) error {
	s, err := newSession(cmd)
	if err != nil {
		return err
	}
	u := s.UI

	settings, err := installSettings(cmd)
	if err != nil {
		return err
	}
	switch settings.Runtime {
	case "", "kubernetes":
	case "docker":
		return dockerInstallAction(ctx, u, settings)
	default:
		return fmt.Errorf("runtime must be kubernetes or docker, not %q", settings.Runtime)
	}
	if err := validateKubernetesSettings(settings); err != nil {
		return err
	}

	kubeContext := s.Resolved.KubeContext
	client, err := k8s.New(kubeContext)
	if err != nil {
		return fmt.Errorf("couldn't connect to the cluster (context %q): %w", kubeContext, err)
	}
	if _, err := k8s.Ping(ctx, kubeContext); err != nil {
		return fmt.Errorf("the cluster (context %q) didn't answer — donkey installs into an existing cluster: %w", kubeContext, err)
	}
	// Everything applied from here carries this installation's identity. An
	// unbound client would stamp the default installation — and the objects of a
	// second installation would then belong, as far as every sweep is
	// concerned, to the first.
	client = client.WithInstallation(settings.Kubernetes.SystemNamespace())

	// Keep existing secrets on re-installs: a re-run without the secret env
	// vars must not blank what the installation depends on. A read error is
	// fatal for the same reason — treating it as "none yet" would rotate the
	// session key and sign everyone out.
	systemNamespace := settings.Kubernetes.SystemNamespace()
	nsPlan, err := planNamespaces(ctx, u, client, settings.Kubernetes)
	if err != nil {
		return err
	}
	if err := checkOptionalCRDs(ctx, client, settings); err != nil {
		return err
	}
	existing, err := client.GetSecretData(ctx, systemNamespace, manifests.SystemConfigSecretName)
	if err != nil {
		return fmt.Errorf("couldn't read the existing installation's secrets: %w", err)
	}
	for _, source := range []struct {
		name  string
		host  string
		field string
	}{
		{settings.Registry.DockerConfigSecret, "", "registry.dockerConfigSecret"},
		{settings.Build.Packages.DockerConfigSecret, settings.Build.Packages.DockerConfigHost, "build.packages.dockerConfigSecret"},
	} {
		if source.name == "" {
			continue
		}
		data, err := client.GetSecretData(ctx, systemNamespace, source.name)
		if err != nil {
			return fmt.Errorf("couldn't read %s %q: %w", source.field, source.name, err)
		}
		if err := validateDockerConfigSecretData(source.field, source.name, data, source.host); err != nil {
			return err
		}
	}
	// Nothing there means a new installation — including when the intent was
	// to update an existing one that lives somewhere else. Saying so is the
	// whole warning: the session key below would be a fresh one, which signs
	// every user out of every app and revokes every `donkey login`.
	if len(existing) == 0 && nsPlan.SkipSystemNamespace {
		u.Warn("No existing donkey installation in %s. This creates a new one — a new session key, so any installation elsewhere keeps its own and everyone signed in here would have to sign in again.", systemNamespace)
	}
	registryUsername, registryPassword, err := settings.Registry.Credentials()
	if err != nil {
		return err
	}
	if registryPassword == "" {
		registryPassword = existing["registry-password"]
		registryUsername = settings.Registry.Username
	}
	if settings.AI.APIKey == "" {
		settings.AI.APIKey = existing["openai-api-key"]
	}
	// The "pending-setup" sentinel marks credentials that were never real —
	// it must not survive into a configuration that now has a real client id.
	if v := existing["oidc-client-secret"]; settings.Auth.ClientSecret == "" && v != pendingOIDC {
		settings.Auth.ClientSecret = v
	}
	if v := existing["oidc-client-id"]; settings.Auth.ClientID == "" && v != pendingOIDC {
		settings.Auth.ClientID = v
	}
	if settings.Build.Packages.Password == "" && settings.Build.Packages.NeedsPassword() {
		settings.Build.Packages.Password = existing["packages-password"]
	}
	if err := settings.Build.Packages.ValidateCredentials(); err != nil {
		return err
	}

	// The AI key may live only in the existing installation — checked here,
	// after the fallback, so a plain re-run doesn't demand env vars the
	// cluster already has.
	if missing := missingSettings(settings, true); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	// Prove the assistant answers before installing anything. An install is a
	// one-shot command with a person watching it, which makes it the cheapest
	// possible place to find out the endpoint is wrong — far cheaper than a
	// server that comes up and fails on its first deploy.
	if err := checkAssistant(ctx, u, settings.AI); err != nil {
		return err
	}

	// The session key seals the gateway's login cookies and the CLI tokens.
	// Reused on re-installs — rotating it signs everyone out of every app
	// and revokes every `donkey login`.
	sessionKey := existing["session-key"]
	if sessionKey == "" {
		if sessionKey, err = randomHexKey(); err != nil {
			return err
		}
	}
	// Exporters live longer than login sessions. Keep their root credential
	// stable across re-installs and separate from deliberate session rotation.
	telemetryKey := existing["telemetry-key"]
	if telemetryKey == "" {
		if telemetryKey, err = randomHexKey(); err != nil {
			return err
		}
	}
	in := kubernetesSystemInput(settings, nsPlan.SkipSystemNamespace, registryUsername, registryPassword, sessionKey, telemetryKey)

	// DuckDB is system state, so unlike an app's optional disk the telemetry
	// PVC must be provisionable before any control-plane objects are applied.
	if hasDefault, err := client.HasDefaultStorageClass(ctx); err == nil && !hasDefault {
		return fmt.Errorf("the cluster has no default StorageClass — donkey-telemetry needs one for its 2 Gi DuckDB volume")
	}

	contextName := kubeContext
	if contextName == "" {
		contextName = "the current kubeconfig context"
	}
	u.Printf("Installing the donkey control plane into %s…", u.Bold(contextName))
	if err := client.Apply(ctx, manifests.SystemObjects(in)); err != nil {
		return fmt.Errorf("the cluster refused the installation: %w", err)
	}
	u.Info("Waiting for the server to come up…")
	if err := client.WaitForRollout(ctx, in.Namespace, "donkey-packages"); err != nil {
		return err
	}
	if err := client.WaitForRollout(ctx, in.Namespace, manifests.TelemetryAppName); err != nil {
		return err
	}
	if err := client.WaitForRollout(ctx, in.Namespace, "donkey-server"); err != nil {
		return err
	}
	// Every app's traffic enters through the donkey gateway, so an install
	// isn't done until it routes.
	if err := client.WaitForRollout(ctx, in.Namespace, manifests.GatewayAppName); err != nil {
		return err
	}
	u.Success("Control plane is running.")

	serverURL := in.Scheme() + "://" + in.ServerHost()
	u.Printf("")
	u.Printf("  API:        %s", u.Bold(serverURL))
	u.Printf("  Builder:    %s", u.Faint(builderSummary(settings.Build)))
	switch {
	case in.Admins.Empty():
		u.Printf("  Admins:     %s", u.Faint("everyone your identity provider signs in (narrow it with auth.admins)"))
	default:
		u.Printf("  Admins:     %s", u.Faint("per auth.admins"))
	}
	u.Printf("")
	u.Printf("Console:  open %s and sign in", u.Bold(serverURL))
	u.Printf("CLI:      %s  %s", u.Bold(fmt.Sprintf("donkey login --server %s", serverURL)), u.Faint("(opens the browser)"))
	u.Printf("Then check the whole setup end to end:  %s", u.Bold("donkey doctor"))

	u.Printf("")
	u.Info("Apps are served at https://<app>.<space>.%s.", in.Domain)
	u.Printf("  %s", u.Faint(fmt.Sprintf(
		"Point *.%s and %s itself at the donkey-gateway service — one wildcard DNS record covers every depth, but never the apex.",
		in.Domain, in.Domain)))
	switch in.TLSMode {
	case "", "acme":
		u.Printf("  %s", u.Faint("TLS: the gateway requests one certificate per hostname via ACME (Let's Encrypt) — it must be reachable from the internet on ports 80 and 443."))
	case "ca":
		u.Printf("  %s", u.Faint(fmt.Sprintf(
			"TLS: the gateway signs certificates with its own CA. Export it once and trust it: kubectl -n %s get secret %s -o jsonpath='{.data.ca\\.crt}' | base64 -d",
			in.Namespace, manifests.GatewayCASecret)))
	case "none":
		u.Printf("  %s", u.Faint("TLS: none at donkey's edge — the cluster gateway in front of it terminates TLS."))
	}

	callback := fmt.Sprintf("%s://%s/oauth2/callback", in.Scheme(), in.Domain)
	u.Printf("")
	u.Printf("Login: OIDC via %s", in.OIDCIssuer)
	if settings.Auth.ClientID == "" {
		u.Warn("No OIDC client configured yet — nobody can sign in (console, CLI or apps) until you register one at the issuer and re-run this install with the client id and secret.")
	}
	u.Printf("  %s", u.Faint(fmt.Sprintf("Callback URL: %s — one sign-in covers the console and every app under %s. Custom domains outside it use https://<domain>/oauth2/callback (register each).", callback, in.Domain)))
	return nil
}

// validateKubernetesSettings is the cluster-independent half of install. It
// is shared by render so a bundle cannot encode configuration the direct
// installer would reject later.
func validateKubernetesSettings(settings *config.InstallFile) error {
	if missing := missingSettings(settings, false); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	if err := settings.Registry.Validate(); err != nil {
		return err
	}
	if err := settings.Registry.ValidateRuntime("kubernetes"); err != nil {
		return err
	}
	switch settings.Gateway.ServiceType {
	case "", "LoadBalancer", "ClusterIP", "NodePort":
	default:
		return fmt.Errorf("gateway.serviceType must be LoadBalancer, ClusterIP or NodePort, not %q", settings.Gateway.ServiceType)
	}
	if err := settings.TLS.Validate(); err != nil {
		return err
	}
	if err := settings.Gateway.HTTPRoute.Validate(); err != nil {
		return err
	}
	if err := settings.Gateway.AppGateway.Validate(); err != nil {
		return err
	}
	if h := settings.ServerHost; h != "" && h != settings.Domain && !strings.HasSuffix(h, "."+settings.Domain) {
		return fmt.Errorf("serverHost %q must be the domain %q or a name under it", h, settings.Domain)
	}
	if err := settings.Auth.Admins.Validate(); err != nil {
		return fmt.Errorf("auth.admins: %w", err)
	}
	if err := settings.Build.Validate(); err != nil {
		return err
	}
	if err := settings.Build.Packages.ValidateRuntime("kubernetes"); err != nil {
		return err
	}
	if err := settings.AddonImages.Validate(); err != nil {
		return err
	}
	if err := config.ValidateAddonImagePullSecrets(settings.AddonImagePullSecrets); err != nil {
		return err
	}
	return settings.Kubernetes.Validate()
}

// kubernetesSystemInput is the single mapping from admin settings to the
// canonical Kubernetes object generator. Cluster install supplies persisted
// secrets; offline render supplies markers and removes Secret objects.
func kubernetesSystemInput(settings *config.InstallFile, skipNamespace bool, registryUsername, registryPassword, sessionKey, telemetryKey string) manifests.SystemInput {
	in := manifests.SystemInput{
		Namespace:            settings.Kubernetes.SystemNamespace(),
		SkipNamespace:        skipNamespace,
		WorkloadNamespace:    settings.Kubernetes.Workload.Namespace,
		NamespacePrefix:      settings.Kubernetes.NamespacePrefix(),
		ServerImage:          settings.Image,
		Domain:               settings.Domain,
		ServerHostname:       settings.ServerHost,
		GatewayServiceType:   settings.Gateway.ServiceType,
		GatewayHTTPNodePort:  settings.Gateway.HTTPNodePort,
		GatewayHTTPSNodePort: settings.Gateway.HTTPSNodePort,
		TLSMode:              settings.TLS.Mode,
		ACMEEmail:            settings.TLS.Email,
		ACMEDirectory:        settings.TLS.Directory,
		HTTPRouteGateway: manifests.GatewayRef{
			Name:        settings.Gateway.HTTPRoute.Name,
			Namespace:   settings.Gateway.HTTPRoute.Namespace,
			SectionName: settings.Gateway.HTTPRoute.SectionName,
		},
		AppGateway:                 settings.Gateway.AppGateway,
		RegistryURL:                settings.Registry.URL,
		RegistryUsername:           registryUsername,
		RegistryCASecret:           settings.Registry.CASecret,
		RegistryDockerConfigSecret: settings.Registry.DockerConfigSecret,
		RegistryPassword:           registryPassword,
		RegistryInsecure:           settings.Registry.Insecure,
		RegistryImmutable:          settings.Registry.Immutable,
		Admins:                     settings.Auth.Admins,
		OIDCIssuer:                 settings.Auth.Issuer,
		OIDCClientID:               settings.Auth.ClientID,
		OIDCClientSecret:           settings.Auth.ClientSecret,
		SessionKey:                 sessionKey,
		TelemetryKey:               telemetryKey,
		DefaultPlatform:            settings.DefaultPlatform,
		OpenAIBaseURL:              settings.AI.BaseURL,
		OpenAIAPIKey:               settings.AI.APIKey,
		AIModel:                    settings.AI.Model,
		Build:                      settings.Build,
		AddonImages:                settings.AddonImages,
		AddonImagePullSecrets:      settings.AddonImagePullSecrets,
		BuildkitImage:              settings.BuildkitImage,
	}
	if in.BuildkitImage == "" {
		in.BuildkitImage = config.DefaultBuildkitImage
	}
	if in.OIDCClientID == "" {
		in.OIDCClientID = pendingOIDC
		in.OIDCClientSecret = pendingOIDC
	}
	return in
}

// builderSummary says how apps that bring no Dockerfile will be built.
func builderSummary(b config.InstallBuild) string {
	s := "donkey writes a Dockerfile for each app"
	if b.VerifyStartup() {
		s += ", and checks the image starts before publishing it"
	}
	return s
}

// pendingOIDC marks an OIDC client that isn't registered yet — logins can't
// complete until the admin re-runs the install with real client credentials.
const pendingOIDC = "pending-setup"

// checkAssistant proves the configured model endpoint answers before the
// installation changes — an install is a one-shot command with a person
// watching it, which makes it the cheapest place to find out the endpoint is
// wrong.
//
// It sets the same two environment variables the server itself reads, because
// the assistant's client takes its endpoint and key from the environment —
// this is the one place a CLI has to stage them, and restoring them afterwards
// keeps the rest of the command honest.
//
// The one thing it will not do is fail on a name it cannot resolve. The URL
// being configured is the one the *server* will use, and the two are not
// always the same place: `host.docker.internal` is how a container reaches
// this machine and means nothing on it, and an in-cluster Service name means
// nothing outside the cluster. A name that does not resolve here is therefore
// no evidence at all, so it warns; anything that does resolve and then refuses,
// rejects the key or does not serve the model is a real answer, and stops the
// install.
func checkAssistant(ctx context.Context, u *ui.UI, cfg config.InstallAI) error {
	for k, v := range map[string]string{
		"OPENAI_BASE_URL": cfg.BaseURL,
		"OPENAI_API_KEY":  cfg.APIKey,
	} {
		if v == "" {
			continue
		}
		if old, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, old)
		} else {
			defer os.Unsetenv(k)
		}
		os.Setenv(k, v)
	}
	model := cfg.Model
	if model == "" {
		model = config.DefaultAIModel
	}
	if host := endpointHost(cfg.BaseURL); host != "" {
		if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
			u.Warn("Can't check the assistant from here: %q doesn't resolve on this machine. That is normal when the endpoint is addressed the way the server will reach it — donkey-server checks it again at startup and refuses to run without it.", host)
			return nil
		}
	}
	if err := ai.Reachable(ctx, model); err != nil {
		return fmt.Errorf("%w\n\nThe installation was not changed", err)
	}
	u.Success("The assistant answered (%s).", model)
	return nil
}

// endpointHost is the hostname of a configured endpoint, or "" when there is
// nothing resolvable to check (no URL, or a bare IP the resolver would pass
// through anyway).
func endpointHost(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	return host
}

// missingSettings lists required settings that are absent. The AI key is
// only checked once the existing installation's secrets were merged in
// (withSecrets) — everything else is checkable before touching the cluster.
func validateDockerConfigSecretData(field, name string, data map[string]string, host string) error {
	if len(data) == 0 {
		return fmt.Errorf("%s %q does not exist or contains no Docker config files", field, name)
	}
	for key, value := range data {
		if _, _, _, err := config.DockerConfigDataCredentials([]byte(value), name+"/"+key, "invalid.invalid"); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	if host == "" {
		return nil
	}
	for key, value := range data {
		username, password, ok, err := config.DockerConfigDataCredentials([]byte(value), name+"/"+key, host)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if ok {
			if username == "" || password == "" {
				return fmt.Errorf("%s: credentials for %s in %s/%s need a non-empty username and password", field, host, name, key)
			}
			return nil
		}
	}
	return fmt.Errorf("%s %q has no credentials for %s", field, name, host)
}

func missingSettings(settings *config.InstallFile, withSecrets bool) []string {
	var missing []string
	if settings.Image == "" {
		missing = append(missing, "image")
	}
	if settings.Registry.URL == "" {
		missing = append(missing, "registry.url")
	}
	if settings.Domain == "" {
		missing = append(missing, "domain")
	}
	// The login gateway is the only way in — console, CLI and apps all
	// authenticate against it, so an installation without an issuer would
	// have no administrators at all.
	if settings.Auth.Issuer == "" {
		missing = append(missing, "auth.issuer (an OIDC provider — it is how admins sign in)")
	}
	// donkey reads every app with its assistant — an installation without one
	// could not deploy anything, so this is a required setting.
	if withSecrets && settings.AI.APIKey == "" {
		missing = append(missing, "ai.apiKey (or the OPENAI_API_KEY environment variable)")
	}
	return missing
}

// randomHexKey makes a 32-byte installation secret, hex-encoded. Session and
// telemetry credentials use separate generated values.
func randomHexKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
