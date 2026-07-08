package main

import (
	"log"
	"os"
	"strings"
	"time"

	"emby-auth-gateway/internal/gateway"
	"emby-auth-gateway/internal/pbsetup"
	"emby-auth-gateway/internal/pbstore"

	_ "emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
)

func main() {
	app := pocketbase.New()
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})
	app.RootCmd.AddCommand(pbsetup.NewCommand(app))

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return e.App.RunAppMigrations()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		cipher, err := gateway.NewCipher(requiredEnv("GATEWAY_SECRET_KEY"))
		if err != nil {
			return err
		}
		basePath := normalizeGatewayBasePath(envDefault("GATEWAY_BASE_PATH", "/emby"))
		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:   strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath: basePath,
			GatewayServerID: envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
		}, pbstore.New(e.App, cipher))

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

		if err := e.App.Cron().Add("gatewayPlaybackEventCleanup", "@hourly", func() {
			if err := cleanupPlaybackEvents(e.App, time.Now().UTC()); err != nil {
				e.App.Logger().Warn("Failed to cleanup playback events", "error", err)
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

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
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

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
