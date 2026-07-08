package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"emby-auth-gateway/internal/gateway"
	"emby-auth-gateway/internal/pbstore"

	_ "emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
)

func main() {
	app := pocketbase.New()
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})

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
		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:   strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath: envDefault("GATEWAY_BASE_PATH", "/emby"),
			GatewayServerID: envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
			HTTPClient:      http.DefaultClient,
		}, pbstore.New(e.App, cipher))

		e.Router.Any("/emby/{path...}", func(re *core.RequestEvent) error {
			gw.ServeHTTP(re.Response, re.Request)
			return nil
		})
		e.Router.Any("/emby", func(re *core.RequestEvent) error {
			gw.ServeHTTP(re.Response, re.Request)
			return nil
		})

		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
