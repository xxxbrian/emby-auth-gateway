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
)

func main() {
	app := pocketbase.New()
	app.RootCmd.Version = version.Version
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})
	app.RootCmd.AddCommand(pbsetup.NewCommand(app))
	app.RootCmd.AddCommand(newVersionCommand())
	registerBackendIdentityDefaults(app)

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return e.App.RunAppMigrations()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		basePath := normalizeGatewayBasePath(envDefault("GATEWAY_BASE_PATH", "/emby"))
		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:            strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath:          basePath,
			GatewayServerID:          envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
			MinResumePct:             envFloatDefault("GATEWAY_MIN_RESUME_PCT", 0),
			MaxResumePct:             envFloatDefault("GATEWAY_MAX_RESUME_PCT", 0),
			MinResumeDurationSeconds: envFloatDefault("GATEWAY_MIN_RESUME_DURATION_SECONDS", 0),
		}, pbstore.New(e.App))

		wildcardPath := basePath + "/{path...}"
		if basePath == "/" {
			wildcardPath = "/{path...}"
		}

		e.Router.Any(wildcardPath, func(re *core.RequestEvent) error {
			gw.ServeHTTP(re.Response, re.Request)
			return nil
		})
		e.Router.Any(basePath, func(re *core.RequestEvent) error {
			gw.ServeHTTP(re.Response, re.Request)
			return nil
		})
		go func() {
			if err := gw.RefreshBackendServerInfo(context.Background()); err != nil {
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
			if err := gw.RefreshBackendServerInfo(context.Background()); err != nil {
				e.App.Logger().Warn("Failed to refresh backend server info", "error", err)
			}
		}); err != nil {
			return err
		}

		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func cleanupPlaybackEvents(app core.App, now time.Time) error {
	cutoff := now.UTC().Add(-6 * time.Hour)
	_, err := app.DB().NewQuery("delete from playback_events where occurred_at < {:cutoff}").Bind(map[string]any{"cutoff": cutoff}).Execute()
	return err
}

func registerBackendIdentityDefaults(app core.App) {
	app.OnRecordCreateExecute("emby_servers").BindFunc(func(e *core.RecordEvent) error {
		identity := gateway.BackendClientIdentity{
			UserAgent: e.Record.GetString("backend_user_agent"),
			Client:    e.Record.GetString("backend_authorization_client"),
			Device:    e.Record.GetString("backend_authorization_device"),
			DeviceID:  e.Record.GetString("backend_authorization_device_id"),
			Version:   e.Record.GetString("backend_authorization_version"),
		}.WithDefaults()
		if strings.TrimSpace(identity.DeviceID) == "" {
			seed := e.Record.Id
			if strings.TrimSpace(seed) == "" {
				seed = e.Record.GetString("name")
			}
			identity.DeviceID = gateway.StableBackendDeviceID(seed)
		}

		e.Record.Set("backend_user_agent", identity.UserAgent)
		e.Record.Set("backend_authorization_client", identity.Client)
		e.Record.Set("backend_authorization_device", identity.Device)
		e.Record.Set("backend_authorization_device_id", identity.DeviceID)
		e.Record.Set("backend_authorization_version", identity.Version)
		return e.Next()
	})
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

func normalizeGatewayBasePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/emby"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if trimmed := strings.TrimRight(value, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}
