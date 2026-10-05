package cli

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/server"
)

func addonCommand() *cli.Command {
	return &cli.Command{
		Name:  "addon",
		Usage: "services like databases — they belong to the space, apps connect to them",
		Commands: []*cli.Command{
			addonAddCommand(),
			addonAttachCommand(),
			addonRemoveCommand(),
			addonListCommand(),
			addonTunnelCommand(),
		},
	}
}

func addonAddCommand() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "connect the app to a service of its space, creating it if needed — e.g. donkey addon add postgres",
		ArgsUsage: "TYPE",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "name for the service (defaults to its type) — two apps naming the same one share it"},
			&cli.StringFlag{Name: "version", Usage: "service version (e.g. 16 for postgres); only used when the service is created"},
			&cli.StringFlag{Name: "storage", Usage: "disk size, e.g. 1Gi; only used when the service is created"},
			&cli.StringSliceFlag{Name: "param", Aliases: []string{"p"}, Usage: "a service parameter as name=value (the catalog lists them), e.g. --param storage=5Gi; repeatable, only used when the service is created"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			app, err := requireApp(s)
			if err != nil {
				return err
			}
			typ := cmd.Args().First()
			if typ == "" {
				return fmt.Errorf("tell donkey what to add, e.g. donkey addon add postgres")
			}
			name := cmd.String("name")
			if name == "" {
				name = typ
			}
			params, err := parseParams(cmd.StringSlice("param"))
			if err != nil {
				return err
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			u := s.UI
			u.Printf("Connecting %s to %s…", u.Bold(app.Name), u.Bold(name))
			u.Info("If the space doesn't have it yet, the first time can take a minute while the cluster downloads the %s image.", typ)
			res, err := api.AddAddon(ctx, s.Space, app.Name, server.AddonRequest{
				Type: typ, Name: name, Version: cmd.String("version"), Storage: cmd.String("storage"), Parameters: params,
			})
			if err != nil {
				return err
			}
			if res.Created {
				u.Success("%s is ready — it belongs to the space %s, and %s is connected.", name, s.Space, app.Name)
				if summary := formatParams(res.Parameters); summary != "" {
					u.Detail("running with %s", summary)
				}
			} else {
				u.Success("%s already runs in this space — %s joined it.", name, app.Name)
			}
			return recordAddonAndRedeploy(ctx, s, cmd, app, config.AddonConfig{
				Name: name, Type: typ, Version: cmd.String("version"), Storage: cmd.String("storage"), Parameters: params,
			})
		},
	}
}

func addonAttachCommand() *cli.Command {
	return &cli.Command{
		Name:      "attach",
		Usage:     "connect the app to a service that already runs elsewhere, e.g. donkey addon attach postgres --url postgres://…",
		ArgsUsage: "TYPE",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "url", Required: true, Usage: "connection address of the existing service"},
			&cli.StringFlag{Name: "name", Usage: "name for this service (defaults to its type)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			app, err := requireApp(s)
			if err != nil {
				return err
			}
			typ := cmd.Args().First()
			if typ == "" {
				return fmt.Errorf("tell donkey what to attach, e.g. donkey addon attach postgres --url …")
			}
			name := cmd.String("name")
			if name == "" {
				name = typ
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			if _, err := api.AddAddon(ctx, s.Space, app.Name, server.AddonRequest{
				Type: typ, Name: name, External: true, URL: cmd.String("url"),
			}); err != nil {
				return err
			}
			s.UI.Success("Connected — the space now knows this %s, and the app sees it exactly like a donkey-managed one.", typ)
			return recordAddonAndRedeploy(ctx, s, cmd, app, config.AddonConfig{
				Name: name, Type: typ, External: true,
			})
		},
	}
}

func addonRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "disconnect the app from a service; --space removes the service (and its data) from the space",
		ArgsUsage: "NAME",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "space", Usage: "remove the service from the space itself, disconnecting every app that uses it"},
			&cli.BoolFlag{Name: "keep-data", Usage: "with --space: keep the stored data (only the running service goes away)"},
			&cli.BoolFlag{Name: "force", Usage: "don't ask for confirmation (for scripts)"},
			&cli.BoolFlag{Name: "orphan", Usage: "with --space: drop donkey's record of a plugin-provided service even when its plugin is gone or failing — the backing service is left behind for the operator"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			app, err := requireApp(s)
			if err != nil {
				return err
			}
			name := cmd.Args().First()
			if name == "" {
				return fmt.Errorf("tell donkey which service — `donkey addon list` shows them")
			}
			if cmd.Bool("orphan") && (!cmd.Bool("space") || cmd.Bool("keep-data")) {
				return fmt.Errorf("--orphan requires --space and cannot be combined with --keep-data")
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			u := s.UI
			if cmd.Bool("space") {
				if !cmd.Bool("keep-data") && !cmd.Bool("orphan") {
					// Data deletion needs an explicit yes (or --force); --yes
					// answers prompts with their default, which here is "no".
					if !cmd.Bool("force") && !u.Confirm(fmt.Sprintf("Remove %s from the space AND permanently delete its data?", name), false) {
						if !u.Interactive || u.AssumeYes {
							return fmt.Errorf("removing %s needs confirmation — pass --force to skip the prompt, or --keep-data to spare its data", name)
						}
						u.Info("Nothing changed. Use --keep-data to remove only the running service.")
						return nil
					}
				}
				if err := api.RemoveSpaceAddon(ctx, s.Space, name, cmd.Bool("keep-data"), cmd.Bool("orphan")); err != nil {
					return err
				}
				u.Success("%s removed from the space.", name)
			} else {
				if err := api.RemoveAddon(ctx, s.Space, app.Name, name); err != nil {
					return err
				}
				u.Success("%s is disconnected from %s. The service keeps running for the space — `donkey addon remove %s --space` removes it entirely.", name, app.Name, name)
			}
			idx := slices.IndexFunc(app.Addons, func(a config.AddonConfig) bool { return a.Name == name })
			if idx < 0 {
				return nil // wasn't in donkey.yaml (connected via the console)
			}
			app.Addons = slices.Delete(app.Addons, idx, idx+1)
			if err := app.Save(s.Resolved.Flags.AppDir); err != nil {
				return err
			}
			return redeployForAddons(ctx, s, cmd, app)
		},
	}
}

func addonListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "show the services of this space and who uses them",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			// The listing is the space's; a project folder only adds the
			// "this app" marker, so none is fine (e.g. with --space).
			appName := ""
			if app, err := requireApp(s); err == nil {
				appName = app.Name
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			services, err := api.SpaceAddons(ctx, s.Space)
			if err != nil {
				return err
			}
			if len(services) == 0 {
				s.UI.Info("The space %s has no services — add a database with `donkey addon add postgres`.", s.Space)
				return nil
			}
			for _, svc := range services {
				users := "not connected to any app yet"
				if len(svc.AttachedTo) > 0 {
					users = "used by " + strings.Join(svc.AttachedTo, ", ")
				}
				marker := ""
				if appName != "" && slices.Contains(svc.AttachedTo, appName) {
					marker = " ← this app"
				}
				// A plugin instance without a binding is not usable yet (or any
				// more); say so instead of listing it like the others.
				state := ""
				if svc.State != "" && svc.State != "ready" {
					state = " [" + svc.State + "]"
					if svc.Message != "" {
						state += " " + svc.Message
					}
				}
				if summary := formatParams(svc.Parameters); summary != "" {
					users += " · " + summary
				}
				s.UI.Printf("%s%s — %s%s", s.UI.Bold(svc.Name), state, users, marker)
			}
			return nil
		},
	}
}

// addonTunnelCommand brings a service to the developer's machine. Services
// rightly have no public address; this is the sanctioned way in — a private
// line through donkey-server that lives only while the command runs.
func addonTunnelCommand() *cli.Command {
	return &cli.Command{
		Name:      "tunnel",
		Usage:     "reach a service from this machine, e.g. donkey addon tunnel postgres — nothing gets exposed",
		ArgsUsage: "NAME",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "port", Usage: "local port to listen on (defaults to the service's own)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			name := cmd.Args().First()
			if name == "" {
				return fmt.Errorf("tell donkey which service — `donkey addon list` shows them")
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			info, err := api.AddonBinding(ctx, s.Space, name)
			if err != nil {
				return err
			}
			if info.Host == "" {
				return fmt.Errorf("%s runs elsewhere — reach it at its own address, no tunnel needed", name)
			}
			localPort := int(cmd.Int("port"))
			if localPort == 0 {
				localPort = info.Port
			}
			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				return fmt.Errorf("port %d is taken on this machine — pick another with --port: %w", localPort, err)
			}
			defer listener.Close()

			u := s.UI
			u.Printf("%s of space %s is now on your machine:", u.Bold(name), u.Bold(s.Space))
			for _, k := range slices.Sorted(maps.Keys(info.Binding)) {
				u.Printf("  %s=%s", k, localizeBinding(k, info.Binding[k], info.Host, info.Port, localPort))
			}
			u.Info("The line lives only while this command runs — Ctrl-C closes it.")

			go func() {
				<-ctx.Done()
				listener.Close()
			}()
			for {
				conn, err := listener.Accept()
				if err != nil {
					if ctx.Err() != nil {
						u.Printf("Tunnel closed.")
						return nil
					}
					return err
				}
				go func() {
					defer conn.Close()
					ws, err := api.DialAddonTunnel(ctx, s.Space, name)
					if err != nil {
						u.Info("A connection couldn't get through: %v", err)
						return
					}
					pipeTunnel(ws, conn)
				}()
			}
		},
	}
}

// parseParams reads --param name=value pairs. Values stay strings: the
// server coerces them against the service's schema, so storage=5Gi,
// version=16 and replicas=3 all land as the type expects.
func parseParams(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("parameters are name=value pairs, e.g. --param storage=5Gi (got %q)", pair)
		}
		out[name] = value
	}
	return out, nil
}

// formatParams renders parameters as "version=18.6, storage=1Gi".
func formatParams(params map[string]any) string {
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(params)) {
		parts = append(parts, fmt.Sprintf("%s=%v", key, params[key]))
	}
	return strings.Join(parts, ", ")
}

// localizeBinding rewrites one binding value so it points at the tunnel's
// local end instead of into the cluster.
func localizeBinding(key, value, host string, remotePort, localPort int) string {
	remote, local := strconv.Itoa(remotePort), strconv.Itoa(localPort)
	switch {
	case key == "PGHOST" || key == "HOST" || strings.HasSuffix(key, "_HOST"):
		if value == host {
			return "localhost"
		}
	case key == "PGPORT" || key == "PORT" || strings.HasSuffix(key, "_PORT"):
		if value == remote {
			return local
		}
	case strings.HasSuffix(key, "_ADDR") || strings.HasSuffix(key, "_ADDRESS"):
		if h, p, err := net.SplitHostPort(value); err == nil && h == host && p == remote {
			return net.JoinHostPort("localhost", local)
		}
	case strings.HasSuffix(key, "_URL") || key == "AWS_ENDPOINT_URL_S3":
		prefix, raw := "", value
		if strings.HasPrefix(raw, "jdbc:") {
			prefix, raw = "jdbc:", strings.TrimPrefix(raw, "jdbc:")
		}
		u, err := url.Parse(raw)
		if err == nil && u.Hostname() == host && (u.Port() == "" || u.Port() == remote) {
			u.Host = net.JoinHostPort("localhost", local)
			return prefix + u.String()
		}
	}
	return value // credentials, URL paths, and other ports are data, not addresses
}

// pipeTunnel shovels bytes between one local TCP connection and its
// WebSocket carriage until either side hangs up.
func pipeTunnel(ws *websocket.Conn, conn net.Conn) {
	defer ws.Close()
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if _, err := conn.Write(msg); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
}

// recordAddonAndRedeploy persists the addon in donkey.yaml (replacing any
// same-name entry) and redeploys so the app picks up the binding.
func recordAddonAndRedeploy(ctx context.Context, s *session, cmd *cli.Command, app *config.AppConfig, entry config.AddonConfig) error {
	replaced := false
	for i, a := range app.Addons {
		if a.Name == entry.Name {
			app.Addons[i] = entry
			replaced = true
		}
	}
	if !replaced {
		app.Addons = append(app.Addons, entry)
	}
	if err := app.Save(s.Resolved.Flags.AppDir); err != nil {
		return fmt.Errorf("couldn't update donkey.yaml: %w", err)
	}
	s.UI.Detail("recorded in donkey.yaml")
	return redeployForAddons(ctx, s, cmd, app)
}

// redeployForAddons re-runs a deploy (server-side, --no-build isn't needed:
// the app was already deployed, and the wiring lives in its Deployment).
func redeployForAddons(ctx context.Context, s *session, cmd *cli.Command, app *config.AppConfig) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	st, err := api.Status(ctx, s.Space, app.Name)
	if err != nil {
		return err
	}
	if !st.Found || st.Image == "" {
		s.UI.Info("Run %s to put the app live with this change.", s.UI.Bold("donkey deploy"))
		return nil
	}
	s.UI.Info("Restarting %s so it sees the change…", app.Name)
	return deployWithTag(ctx, s, cmd, st.Image)
}
