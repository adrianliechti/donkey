package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func serverCommand() *cli.Command {
	return &cli.Command{
		Name:  "server",
		Usage: "manage the donkey control plane in your cluster",
		Commands: []*cli.Command{
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
		Name:  "install",
		Usage: "install (or update) the donkey control plane into an existing cluster",
		Flags: []cli.Flag{
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
			&cli.StringFlag{Name: "builder", Usage: "how apps without a Dockerfile are built: railpack (default) or ai (donkey writes the Dockerfile; needs no egress beyond the registry)"},
			&cli.BoolFlag{Name: "no-verify", Usage: "skip running a freshly built image to check it starts (builder ai)"},
			&cli.StringFlag{Name: "railpack-builder-image", Usage: "railpack builder image (mirror it for a restricted network)"},
			&cli.StringFlag{Name: "railpack-runtime-image", Usage: "railpack runtime image (mirror it for a restricted network)"},
			&cli.StringFlag{Name: "https-proxy", Usage: "forward proxy for donkey-server and the build engine"},
			&cli.StringFlag{Name: "no-proxy", Usage: "hosts that bypass the forward proxy, comma-separated"},
			&cli.StringFlag{Name: "tls-mode", Usage: "how the gateway gets certificates: acme (default), ca (built-in CA for local/internal installs) or static (bring your own)"},
			&cli.StringFlag{Name: "acme-email", Usage: "ACME account contact for expiry warnings (tls-mode acme)"},
			&cli.StringFlag{Name: "acme-directory", Usage: "ACME endpoint override, e.g. Let's Encrypt staging (tls-mode acme)"},
			&cli.StringFlag{Name: "tls-secret", Usage: "kubernetes.io/tls Secret in donkey-system with the certificate (tls-mode static)"},
			&cli.StringFlag{Name: "oidc-issuer", Usage: "OIDC issuer for the login gateway (e.g. Entra ID); empty = no gateway"},
			&cli.StringFlag{Name: "oidc-client-id", Usage: "OIDC client id for the gateway"},
			&cli.StringFlag{Name: "oidc-client-secret", Usage: "OIDC client secret (or $DONKEY_OIDC_CLIENT_SECRET)"},
			&cli.StringFlag{Name: "openai-base-url", Usage: "OpenAI-compatible endpoint for AI assistance (as reachable FROM the cluster)"},
			&cli.StringFlag{Name: "openai-api-key", Usage: "API key for AI assistance (or $OPENAI_API_KEY)"},
			&cli.StringFlag{Name: "ai-model", Usage: "model for AI assistance"},
		},
		Action: serverInstallAction,
	}
}

// installSettings merges the settings file with flag overrides.
func installSettings(cmd *cli.Command) (*config.InstallFile, error) {
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
	if v := cmd.String("builder"); v != "" {
		settings.Build.Builder = config.BuildStrategy(v)
	}
	if cmd.Bool("no-verify") {
		settings.Build.AI.Verify = new(false)
	}
	strOver(&settings.Build.Railpack.BuilderImage, "railpack-builder-image")
	strOver(&settings.Build.Railpack.RuntimeImage, "railpack-runtime-image")
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
	strOver(&settings.TLS.Secret, "tls-secret")
	if cmd.Bool("registry-insecure") {
		settings.Registry.Insecure = true
	}
	if cmd.Bool("registry-immutable") {
		settings.Registry.Immutable = true
	}
	// Secrets may come from env instead of file/flags.
	if settings.Auth.ClientSecret == "" {
		settings.Auth.ClientSecret = os.Getenv("DONKEY_OIDC_CLIENT_SECRET")
	}
	if settings.AI.APIKey == "" {
		settings.AI.APIKey = os.Getenv("OPENAI_API_KEY")
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
	// Everything checkable without the cluster is checked first — a missing
	// kubeconfig must not hide the list of missing settings.
	if missing := missingSettings(settings, false); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
	}
	if err := settings.Registry.Validate(); err != nil {
		return err
	}
	switch settings.Gateway.ServiceType {
	case "", "LoadBalancer", "ClusterIP", "NodePort":
	default:
		return fmt.Errorf("gateway.serviceType must be LoadBalancer, ClusterIP or NodePort, not %q", settings.Gateway.ServiceType)
	}
	switch settings.TLS.Mode {
	case "", "acme", "ca", "static":
	default:
		return fmt.Errorf("tls.mode must be acme, ca or static, not %q", settings.TLS.Mode)
	}
	// The central login callback lives on the server host, and its session
	// cookie's domain is the base domain — a server host outside it could
	// never complete a login.
	if h := settings.ServerHost; h != "" && h != settings.Domain && !strings.HasSuffix(h, "."+settings.Domain) {
		return fmt.Errorf("serverHost %q must be the domain %q or a name under it", h, settings.Domain)
	}
	if err := settings.Auth.Admins.Validate(); err != nil {
		return fmt.Errorf("auth.admins: %w", err)
	}
	if err := settings.Build.Validate(); err != nil {
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

	// Keep existing secrets on re-installs so logins and data stay valid — a
	// re-run without $DONKEY_REGISTRY_PASSWORD (or the other secret env vars)
	// set must never blank what the running installation depends on. A read
	// error is fatal for the same reason: treating it as "no secrets yet"
	// would silently rotate the session key and sign everyone out.
	existing, err := client.GetSecretData(ctx, manifests.SystemNamespace, "donkey-system-config")
	if err != nil {
		return fmt.Errorf("couldn't read the existing installation's secrets: %w", err)
	}
	registryPassword, err := settings.Registry.Password() // env, then registry.passwordFile
	if err != nil {
		return err
	}
	if registryPassword == "" {
		registryPassword = existing["registry-password"]
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

	// The AI key may live only in the existing installation — checked here,
	// after the fallback, so a plain re-run doesn't demand env vars the
	// cluster already has.
	if missing := missingSettings(settings, true); len(missing) > 0 {
		return fmt.Errorf("these settings are required (via -f file or flags): %v", missing)
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
	in := manifests.SystemInput{
		Namespace:            manifests.SystemNamespace,
		ServerImage:          settings.Image,
		Domain:               settings.Domain,
		ServerHostname:       settings.ServerHost,
		GatewayServiceType:   settings.Gateway.ServiceType,
		GatewayHTTPNodePort:  settings.Gateway.HTTPNodePort,
		GatewayHTTPSNodePort: settings.Gateway.HTTPSNodePort,
		TLSMode:              settings.TLS.Mode,
		ACMEEmail:            settings.TLS.Email,
		ACMEDirectory:        settings.TLS.Directory,
		TLSSecret:            settings.TLS.Secret,
		RegistryURL:          settings.Registry.URL,
		RegistryUsername:     settings.Registry.Username,
		RegistryCASecret:     settings.Registry.CASecret,
		RegistryPassword:     registryPassword,
		RegistryInsecure:     settings.Registry.Insecure,
		RegistryImmutable:    settings.Registry.Immutable,
		Admins:               settings.Auth.Admins,
		OIDCIssuer:           settings.Auth.Issuer,
		OIDCClientID:         settings.Auth.ClientID,
		OIDCClientSecret:     settings.Auth.ClientSecret,
		SessionKey:           sessionKey,
		DefaultPlatform:      settings.DefaultPlatform,
		OpenAIBaseURL:        settings.AI.BaseURL,
		OpenAIAPIKey:         settings.AI.APIKey,
		AIModel:              settings.AI.Model,
		Build:                settings.Build,
		BuildkitImage:        settings.BuildkitImage,
		NamespacePrefix:      "donkey-",
	}
	if in.BuildkitImage == "" {
		in.BuildkitImage = config.DefaultBuildkitImage
	}
	if in.OIDCClientID == "" {
		in.OIDCClientID = pendingOIDC
		in.OIDCClientSecret = pendingOIDC
	}

	// The one preflight left: disks. TLS and routing are donkey's own now —
	// no Envoy Gateway, no cert-manager to check for.
	if hasDefault, err := client.HasDefaultStorageClass(ctx); err == nil && !hasDefault {
		u.Warn("The cluster has no default StorageClass — apps with storage and provisioned addons won't get disks until one exists (or storage.class is set).")
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
			manifests.SystemNamespace, manifests.GatewayCASecret)))
	case "static":
		u.Printf("  %s", u.Faint(fmt.Sprintf("TLS: served from the certificate in Secret %s/%s.", manifests.SystemNamespace, in.TLSSecret)))
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

// builderSummary says how apps that bring no Dockerfile will be built, and
// what that means for what the installation has to reach.
func builderSummary(b config.InstallBuild) string {
	switch b.BuilderOrDefault() {
	case config.BuildAI:
		s := "donkey writes a Dockerfile for each app"
		if b.VerifyStartup() {
			s += ", and checks the image starts before publishing it"
		}
		return s
	default:
		return "railpack (needs egress to github.com to resolve package versions; build.builder: ai does not)"
	}
}

// pendingOIDC marks an OIDC client that isn't registered yet — logins can't
// complete until the admin re-runs the install with real client credentials.
const pendingOIDC = "pending-setup"

// missingSettings lists required settings that are absent. The AI key is
// only checked once the existing installation's secrets were merged in
// (withSecrets) — everything else is checkable before touching the cluster.
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

// randomHexKey is the installation's sealing key — every session cookie and
// CLI token hangs off it: 32 bytes, hex-encoded.
func randomHexKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
