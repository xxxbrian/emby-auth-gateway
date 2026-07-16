package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbsetup"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbstore"
	"github.com/xxxbrian/emby-auth-gateway/internal/version"

	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/spf13/cobra"
)

func main() {
	if code := runGateway(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

func runGateway(args []string) int {
	app := newGatewayApp()
	app.RootCmd.SetArgs(args)

	// PocketBase's Start/Execute discard cobra command errors (exit 0 on failure).
	// Commands that execute directly must propagate a nonzero process status so
	// Compose service_completed_successfully and scripts can trust failure.
	if selectsDirectExecution(app, args) {
		if err := app.RootCmd.Execute(); err != nil {
			return 1
		}
		return 0
	}

	if err := app.Start(); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

func newGatewayApp() *pocketbase.PocketBase {
	app := pocketbase.New()
	app.RootCmd.Version = version.Version
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})
	app.RootCmd.AddCommand(pbsetup.NewCommand(app))
	versionCommand := newVersionCommand()
	versionCommand.Args = cobra.NoArgs
	versionCommand.FParseErrWhitelist.UnknownFlags = false
	app.RootCmd.AddCommand(versionCommand)
	webCommand := newWebCommand()
	configureDirectCommandGroup(webCommand)
	app.RootCmd.AddCommand(webCommand)
	registerActivityLogTokenRedaction(app)

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return e.App.RunAppMigrations()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:            strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath:          fixedGatewayBasePath,
			GatewayServerID:          envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
			MinResumePct:             envFloatDefault("GATEWAY_MIN_RESUME_PCT", 0),
			MaxResumePct:             envFloatDefault("GATEWAY_MAX_RESUME_PCT", 0),
			MinResumeDurationSeconds: envFloatDefault("GATEWAY_MIN_RESUME_DURATION_SECONDS", 0),
		}, pbstore.New(e.App))
		web, err := newEmbyWebServer(webAssetsDirFromEnv(), os.Getenv("GATEWAY_PUBLIC_URL"))
		if err != nil {
			return err
		}

		mountGatewayRoutes(e.Router, web, gw, webReadyForRootRedirect(web))

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

// selectsDirectExecution reports whether args identify a direct command subtree.
// Once Cobra resolves a direct top-level command, later malformed arguments must
// still execute RootCmd directly so its error reaches the process status.
func selectsDirectExecution(app *pocketbase.PocketBase, args []string) bool {
	cmd, _, _ := app.RootCmd.Find(args)
	if isDirectCommand(cmd, app.RootCmd) {
		return true
	}

	// Find normally handles root persistent flags. This fallback only covers the
	// supported --dir spellings before a top-level command.
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--dir" {
			i++
			if i == len(args) {
				return false
			}
			continue
		}
		if strings.HasPrefix(arg, "--dir=") {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return false
		}
		return isDirectCommandName(arg)
	}
	return false
}

func isDirectCommand(cmd, root *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	for cmd.HasParent() && cmd.Parent() != root {
		cmd = cmd.Parent()
	}
	return isDirectCommandName(cmd.Name())
}

func isDirectCommandName(name string) bool {
	switch name {
	case "setup", "web", "version":
		return true
	default:
		return false
	}
}

// configureDirectCommandGroup makes a command group safe for direct execution:
// no-argument invocations render help, while unresolved children are rejected
// by Cobra before any subcommand can run.
func configureDirectCommandGroup(cmd *cobra.Command) {
	cmd.Args = cobra.NoArgs
	cmd.FParseErrWhitelist.UnknownFlags = false
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	}
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
