package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/k8s"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
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
			&cli.StringFlag{Name: "tls-issuer", Usage: "cert-manager issuer that signs app certificates (required)"},
			&cli.StringFlag{Name: "tls-issuer-kind", Usage: "ClusterIssuer (default) or Issuer"},
			&cli.StringFlag{Name: "oidc-issuer", Usage: "OIDC issuer for the login gateway (e.g. Entra ID); empty = no gateway"},
			&cli.StringFlag{Name: "oidc-client-id", Usage: "OIDC client id for the gateway"},
			&cli.StringFlag{Name: "oidc-client-secret", Usage: "OIDC client secret (or $DONKEY_OIDC_CLIENT_SECRET)"},
			&cli.StringFlag{Name: "openai-base-url", Usage: "OpenAI-compatible endpoint for AI assistance (as reachable FROM the cluster)"},
			&cli.StringFlag{Name: "openai-api-key", Usage: "API key for AI assistance (or $OPENAI_API_KEY)"},
			&cli.StringFlag{Name: "ai-model", Usage: "model for AI assistance"},
			&cli.StringFlag{Name: "api-token", Usage: "fix the API token (default: generate one)"},
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
	strOver(&settings.Image, "image")
	strOver(&settings.BuildkitImage, "buildkit-image")
	strOver(&settings.Registry.URL, "registry-url")
	strOver(&settings.Registry.Username, "registry-username")
	strOver(&settings.Registry.CASecret, "registry-ca-secret")
	strOver(&settings.Domain, "domain")
	strOver(&settings.ServerHost, "server-host")
	strOver(&settings.Gateway.ServiceType, "gateway-service-type")
	strOver(&settings.DefaultPlatform, "default-platform")
	strOver(&settings.Auth.Issuer, "oidc-issuer")
	strOver(&settings.Auth.ClientID, "oidc-client-id")
	strOver(&settings.Auth.ClientSecret, "oidc-client-secret")
	strOver(&settings.AI.BaseURL, "openai-base-url")
	strOver(&settings.AI.APIKey, "openai-api-key")
	strOver(&settings.AI.Model, "ai-model")
	strOver(&settings.TLS.Issuer, "tls-issuer")
	strOver(&settings.TLS.Kind, "tls-issuer-kind")
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
	// The central login callback lives on the server host, and its session
	// cookie's domain is the base domain — a server host outside it could
	// never complete a login.
	if h := settings.ServerHost; h != "" && h != settings.Domain && !strings.HasSuffix(h, "."+settings.Domain) {
		return fmt.Errorf("serverHost %q must be the domain %q or a name under it", h, settings.Domain)
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
	// would silently rotate the API token.
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

	apiToken := cmd.String("api-token")
	tokenFromFlag := apiToken != ""
	newToken := false
	if apiToken == "" {
		if apiToken = existing["api-token"]; apiToken == "" {
			if apiToken, err = randomToken(); err != nil {
				return err
			}
			newToken = true
		}
	}
	// The session key seals the gateway's login cookies. Reused on
	// re-installs — rotating it would sign everyone out of every app.
	sessionKey := existing["session-key"]
	if sessionKey == "" {
		if sessionKey, err = randomHexKey(); err != nil {
			return err
		}
	}
	in := manifests.SystemInput{
		Namespace:          manifests.SystemNamespace,
		ServerImage:        settings.Image,
		Domain:             settings.Domain,
		ServerHostname:     settings.ServerHost,
		GatewayServiceType: settings.Gateway.ServiceType,
		TLSIssuer:          settings.TLS.Issuer,
		TLSIssuerKind:      settings.TLS.Kind,
		RegistryURL:        settings.Registry.URL,
		RegistryUsername:   settings.Registry.Username,
		RegistryCASecret:   settings.Registry.CASecret,
		RegistryPassword:   registryPassword,
		RegistryInsecure:   settings.Registry.Insecure,
		RegistryImmutable:  settings.Registry.Immutable,
		APIToken:           apiToken,
		OIDCIssuer:         settings.Auth.Issuer,
		OIDCClientID:       settings.Auth.ClientID,
		OIDCClientSecret:   settings.Auth.ClientSecret,
		SessionKey:         sessionKey,
		DefaultPlatform:    settings.DefaultPlatform,
		OpenAIBaseURL:      settings.AI.BaseURL,
		OpenAIAPIKey:       settings.AI.APIKey,
		AIModel:            settings.AI.Model,
		BuildkitImage:      settings.BuildkitImage,
		NamespacePrefix:    "donkey-",
	}
	if in.BuildkitImage == "" {
		in.BuildkitImage = config.DefaultBuildkitImage
	}
	if in.OIDCClientID == "" {
		in.OIDCClientID = pendingOIDC
		in.OIDCClientSecret = pendingOIDC
	}

	if in.TLSIssuerKind == "" {
		in.TLSIssuerKind = "ClusterIssuer"
	}

	// The prerequisites, checked before anything is applied: Envoy Gateway
	// (everything donkey serves attaches to it), cert-manager (below), and —
	// as a warning — a default StorageClass for apps and addons with disks.
	if err := client.GatewayAPIAvailable(ctx); err != nil {
		return fmt.Errorf("%w\n\nInstall it first: https://gateway.envoyproxy.io/docs/tasks/quickstart/", err)
	}
	if hasDefault, err := client.HasDefaultStorageClass(ctx); err == nil && !hasDefault {
		u.Warn("The cluster has no default StorageClass — apps with storage and provisioned addons won't get disks until one exists (or storage.class is set).")
	}
	// Every deployed app has its certificate on the gateway's https listener.
	// Re-installing renders that listener again, so carry the existing
	// certificates over — otherwise a re-install would take https away from
	// every running app until each one redeploys.
	refs, err := client.GatewayCertificateRefs(ctx, manifests.SystemNamespace, manifests.GatewayName)
	if err != nil {
		return fmt.Errorf("couldn't read the existing gateway: %w", err)
	}
	in.TLSCertificateRefs = refs

	if err := client.CertManagerInstalled(ctx); err != nil {
		return fmt.Errorf("%w\n\nInstall it first: kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml", err)
	}
	if ready, detail, err := client.IssuerReady(ctx, in.TLSIssuerKind, in.TLSIssuer, manifests.SystemNamespace); err != nil {
		return fmt.Errorf("couldn't find the %s %q that should sign app certificates: %w", in.TLSIssuerKind, in.TLSIssuer, err)
	} else if !ready {
		u.Warn("The %s %q isn't ready yet (%s) — apps will deploy, but https starts working once it is.", in.TLSIssuerKind, in.TLSIssuer, detail)
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
	// Every app's traffic rides through the donkey gateway, so an install
	// isn't done until it routes.
	if err := client.WaitForRollout(ctx, in.Namespace, manifests.GatewayAppName); err != nil {
		return err
	}
	u.Success("Control plane is running.")

	// The Gateway only serves once Envoy Gateway programs it — say so when
	// that hasn't happened, instead of printing an API URL that won't answer.
	if reason := waitForGateway(ctx, client); reason != "" {
		u.Warn("Envoy Gateway hasn't picked up the donkey gateway yet (%s) — the URLs below start working once it does.", reason)
	}

	serverURL := in.Scheme() + "://" + in.ServerHost()
	u.Printf("")
	u.Printf("  API:        %s", u.Bold(serverURL))
	switch {
	case newToken:
		u.Printf("  API token:  %s", u.Bold(apiToken))
		u.Printf("  %s", u.Faint("Store this token safely — it is shown only once."))
	case tokenFromFlag:
		u.Printf("  API token:  %s", u.Faint("(as provided — replaces the previous one)"))
	default:
		u.Printf("  API token:  %s", u.Faint("(unchanged)"))
	}
	u.Printf("")
	u.Printf("Connect this machine:  %s", u.Bold(fmt.Sprintf("donkey login --server %s --token <token>", serverURL)))
	u.Printf("Then check the whole setup end to end:  %s", u.Bold("donkey doctor"))

	u.Printf("")
	u.Info("Apps are served at https://<app>.<space>.%s.", in.Domain)
	u.Printf("  %s", u.Faint(fmt.Sprintf(
		"%s %s signs one certificate per app; point *.%s and %s itself at the gateway — one wildcard covers every depth, but never the apex.",
		in.TLSIssuerKind, in.TLSIssuer, in.Domain, in.Domain)))

	callback := fmt.Sprintf("%s://%s/oauth2/callback", in.Scheme(), in.Domain)
	switch {
	case in.OIDCIssuer != "" && settings.Auth.ClientID == "":
		u.Printf("")
		u.Printf("Login protection: OIDC via %s", in.OIDCIssuer)
		u.Warn("No OIDC client configured yet — logins can't complete until you register one at the issuer and re-run this install with the client id and secret.")
		u.Printf("  %s", u.Faint(fmt.Sprintf("Register one callback URL: %s — every login under %s rides through it. Apps with a custom domain outside it keep https://<domain>/oauth2/callback.", callback, in.Domain)))
	case in.OIDCIssuer != "":
		u.Printf("")
		u.Printf("Login protection: OIDC via %s", in.OIDCIssuer)
		u.Printf("  %s", u.Faint(fmt.Sprintf("Callback URL: %s — one sign-in covers every app under %s. Custom domains outside it use https://<domain>/oauth2/callback (register each).", callback, in.Domain)))
	default:
		u.Warn("No login protection configured — deployed apps are reachable without login.")
		u.Printf("  %s", u.Faint("Set auth.issuer (e.g. Entra ID, or any OIDC provider) to protect them."))
	}
	return nil
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
	// Apps are always served over https, and cert-manager issues the
	// certificates — donkey never mints its own.
	if settings.TLS.Issuer == "" {
		missing = append(missing, "tls.issuer (a cert-manager ClusterIssuer or Issuer)")
	}
	// donkey reads every app with its assistant — an installation without one
	// could not deploy anything, so this is a required setting.
	if withSecrets && settings.AI.APIKey == "" {
		missing = append(missing, "ai.apiKey (or the OPENAI_API_KEY environment variable)")
	}
	return missing
}

// waitForGateway gives the controller a moment to program the shared Gateway
// and returns why it hasn't ("" once it has). Non-fatal: DNS or the
// controller may simply still be on their way.
func waitForGateway(ctx context.Context, client *k8s.Client) string {
	deadline := time.Now().Add(30 * time.Second)
	reason := "no status yet"
	for time.Now().Before(deadline) {
		ok, detail, err := client.GatewayProgrammed(ctx, manifests.SystemNamespace, manifests.GatewayName)
		if err == nil && ok {
			return ""
		}
		if err != nil {
			reason = err.Error()
		} else if detail != "" {
			reason = detail
		}
		select {
		case <-ctx.Done():
			return reason
		case <-time.After(2 * time.Second):
		}
	}
	return reason
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dk_" + hex.EncodeToString(b), nil
}

// randomHexKey is the gateway's cookie-sealing key: 32 bytes, hex-encoded.
func randomHexKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
