package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbsetup"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbstore"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/version"

	"github.com/pocketbase/pocketbase"
	pbcmd "github.com/pocketbase/pocketbase/cmd"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func main() {
	if code := runGateway(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

func runGateway(args []string) int {
	validation := newCLIAppForRun()
	mode, err := validateCommand(validation, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	app := newCLIAppForRun()
	app.RootCmd.SetArgs(args)
	switch mode {
	case commandServe:
		if err := app.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case commandSuperuser:
		if err := app.Bootstrap(); err != nil {
			fmt.Fprintln(os.Stderr, errors.Join(err, terminateAndReset(app)))
			return 1
		}
		err := app.RootCmd.Execute()
		if cleanupErr := terminateAndReset(app); err != nil || cleanupErr != nil {
			if cleanupErr != nil {
				fmt.Fprintln(os.Stderr, cleanupErr)
			}
			return 1
		}
		return 0
	default:
		err := app.RootCmd.Execute()
		if cleanupErr := terminateAndReset(app); err != nil || cleanupErr != nil {
			if cleanupErr != nil {
				fmt.Fprintln(os.Stderr, cleanupErr)
			}
			return 1
		}
		return 0
	}
}

type commandMode uint8

const (
	commandDirect commandMode = iota
	commandServe
	commandSuperuser
)

var newCLIAppForRun = newCLIApp

func newCLIApp() *pocketbase.PocketBase {
	app := newGatewayApp()
	registerSystemCommands(app)
	app.RootCmd.FParseErrWhitelist.UnknownFlags = false
	return app
}

func registerSystemCommands(app *pocketbase.PocketBase) {
	for _, command := range app.RootCmd.Commands() {
		if command.Name() == "serve" || command.Name() == "superuser" {
			panic("PocketBase system command registered twice")
		}
	}
	app.RootCmd.AddCommand(pbcmd.NewServeCommand(app, true), pbcmd.NewSuperuserCommand(app))
}

// validateCommand resolves syntax against a disposable command tree. It never
// executes a command or bootstraps an application.
func validateCommand(app *pocketbase.PocketBase, args []string) (commandMode, error) {
	root := app.RootCmd
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	command, remaining, err := root.Find(args)
	if err != nil {
		return commandDirect, err
	}
	if err := command.ParseFlags(remaining); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return commandDirect, nil
		}
		return commandDirect, err
	}
	if commandFlagRequested(root, "help") || commandFlagRequested(root, "version") || commandFlagRequested(command, "help") {
		return commandDirect, nil
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return commandDirect, err
	}
	positionals := command.Flags().Args()
	if command.Args != nil {
		if err := command.ValidateArgs(positionals); err != nil {
			return commandDirect, err
		}
	}
	if command.HasSubCommands() && command.Run == nil && command.RunE == nil {
		if len(positionals) > 0 || command.Name() == "superuser" {
			return commandDirect, fmt.Errorf("unknown or incomplete command %q", command.CommandPath())
		}
	}
	if command == root {
		return commandDirect, nil
	}
	for command.HasParent() && command.Parent() != root {
		command = command.Parent()
	}
	switch command.Name() {
	case "serve":
		return commandServe, nil
	case "superuser":
		return commandSuperuser, nil
	default:
		return commandDirect, nil
	}
}

func commandFlagRequested(command *cobra.Command, name string) bool {
	flag := command.Flags().Lookup(name)
	return flag != nil && flag.Changed
}

func terminateAndReset(app *pocketbase.PocketBase) error {
	event := &core.TerminateEvent{App: app}
	terminateErr := app.OnTerminate().Trigger(event, func(*core.TerminateEvent) error { return nil })
	resetErr := app.ResetBootstrapState()
	return errors.Join(terminateErr, resetErr)
}

func newGatewayApp() *pocketbase.PocketBase {
	app := pocketbase.New()
	app.RootCmd.Version = version.Version
	app.RootCmd.AddCommand(pbsetup.NewCommand(app))
	versionCommand := newVersionCommand()
	versionCommand.Args = cobra.NoArgs
	versionCommand.FParseErrWhitelist.UnknownFlags = false
	app.RootCmd.AddCommand(versionCommand)
	registerActivityLogTokenRedaction(app)

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return pbschema.Ensure(e.App)
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		startedAt := time.Now().UTC()
		emitter := observe.NewEmitter(1024)
		registry := telemetry.New(emitter)
		// Telemetry consumer is best-effort; never block gateway start.
		go registry.Start(context.Background())

		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:            strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath:          fixedGatewayBasePath,
			GatewayServerID:          envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
			MinResumePct:             envFloatDefault("GATEWAY_MIN_RESUME_PCT", 0),
			MaxResumePct:             envFloatDefault("GATEWAY_MAX_RESUME_PCT", 0),
			MinResumeDurationSeconds: envFloatDefault("GATEWAY_MIN_RESUME_DURATION_SECONDS", 0),
			Emitter:                  emitter,
			Meter:                    registry.Meter(),
		}, pbstore.New(e.App))
		web, err := newEmbyWebServer(webAssetsDirFromEnv(), os.Getenv("GATEWAY_PUBLIC_URL"))
		if err != nil {
			return err
		}

		webReady := webReadyForRootRedirect(web)
		mountGatewayRoutes(e.Router, web, gw, webReady)

		adminCfg := adminConfigFromEnv()
		// Exclusive reconfigure gate: media copies (RWMutex) + active playbacks.
		// force=false fails immediately if copies or playbacks are active;
		// force=true waits for copies to drain (playbacks are not waited on).
		acquireReconfigure := func(force bool) (func(), error) {
			if !force && registry != nil && len(registry.ActivePlaybacks()) > 0 {
				return nil, gateway.ErrActiveMedia
			}
			return gw.TryAcquireReconfigure(force)
		}
		if err := mountAdmin(e.Router, e.App, adminCfg, registry, nil, acquireReconfigure, webReady, startedAt, registry.Snapshot().BootID); err != nil {
			return err
		}

		go func() {
			if err := gw.ValidateAnonymousImageNamespace(context.Background()); err != nil {
				e.App.Logger().Warn("Anonymous image namespace unavailable", "error", err)
			}
			if err := gw.RefreshUpstreamServerInfo(context.Background()); err != nil {
				e.App.Logger().Warn("Failed to refresh backend server info", "error", err)
			}
		}()

		if err := e.App.Cron().Add("gatewayPlaybackEventCleanup", "@hourly", func() {
			if err := cleanupPlaybackEvents(e.App, time.Now().UTC()); err != nil {
				e.App.Logger().Warn("Failed to cleanup playback events", "error", err)
			}
			if err := cleanupGatewaySessions(e.App, time.Now().UTC()); err != nil {
				e.App.Logger().Warn("Failed to cleanup gateway sessions", "error", err)
			}
			if err := gw.RefreshUpstreamServerInfo(context.Background()); err != nil {
				e.App.Logger().Warn("Failed to refresh backend server info", "error", err)
			}
		}); err != nil {
			return err
		}

		if err := e.App.Cron().Add("gatewayAuditLogCleanup", "@daily", func() {
			if err := cleanupAuditLogs(e.App, time.Now().UTC(), adminCfg.AuditRetentionDays); err != nil {
				e.App.Logger().Warn("Failed to cleanup audit logs", "error", err)
			}
		}); err != nil {
			return err
		}

		// Build the ServeMux (and set e.Server.Handler), then wrap with the
		// package-owned raw-path guard so traversal cannot clean into API routes.
		if err := e.Next(); err != nil {
			return err
		}
		wrapServerHandler(e.Server)
		return nil
	})

	return app
}

func cleanupPlaybackEvents(app core.App, now time.Time) error {
	cutoff := now.UTC().Add(-6 * time.Hour)
	_, err := app.DB().NewQuery("delete from playback_events where occurred_at < {:cutoff}").Bind(map[string]any{"cutoff": cutoff}).Execute()
	return err
}

func cleanupGatewaySessions(app core.App, now time.Time) error {
	cutoff := now.UTC().Add(-7 * 24 * time.Hour)
	_, err := app.DB().NewQuery("delete from gateway_sessions where expires_at < {:cutoff} or (revoked_at is not null and revoked_at != '' and revoked_at < {:cutoff})").Bind(map[string]any{"cutoff": cutoff}).Execute()
	return err
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envFloatDefault(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
