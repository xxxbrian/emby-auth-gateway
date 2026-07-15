package main

import (
	"context"
	"fmt"
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
	app.RootCmd.AddCommand(newWebCommand())
	registerBackendIdentityDefaults(app)
	registerMappingSessionRevocation(app)
	registerActivityLogTokenRedaction(app)

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return e.App.RunAppMigrations()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		anonymousImageConfig, err := anonymousImageConfigFromEnv()
		if err != nil {
			return err
		}
		gw := gateway.NewServer(gateway.Config{
			PublicBaseURL:                 strings.TrimRight(os.Getenv("GATEWAY_PUBLIC_URL"), "/"),
			GatewayBasePath:               fixedGatewayBasePath,
			GatewayServerID:               envDefault("GATEWAY_SERVER_ID", "emby-auth-gateway"),
			MinResumePct:                  envFloatDefault("GATEWAY_MIN_RESUME_PCT", 0),
			MaxResumePct:                  envFloatDefault("GATEWAY_MAX_RESUME_PCT", 0),
			MinResumeDurationSeconds:      envFloatDefault("GATEWAY_MIN_RESUME_DURATION_SECONDS", 0),
			AnonymousImageServerRecordID:  anonymousImageConfig.serverRecordID,
			AnonymousImageBackendServerID: anonymousImageConfig.backendServerID,
			AnonymousImageConfigured:      anonymousImageConfig.configured,
		}, pbstore.New(e.App))
		web, err := newEmbyWebServer(webAssetsDirFromEnv(), os.Getenv("GATEWAY_PUBLIC_URL"))
		if err != nil {
			return err
		}

		transient, err := startAnonymousImageNamespace(gw, func() {
			mountGatewayRoutes(e.Router, web, gw, webReadyForRootRedirect(web))
		})
		if err != nil {
			return err
		}
		if transient {
			e.App.Logger().Warn("Anonymous image namespace unavailable", "error", "probe unavailable")
		}

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

		// Build the ServeMux (and set e.Server.Handler), then wrap with the
		// package-owned raw-path guard so traversal cannot clean into API routes.
		if err := e.Next(); err != nil {
			return err
		}
		wrapServerHandler(e.Server)
		return nil
	})

	// PocketBase's Start/Execute discard cobra command errors (exit 0 on failure).
	// Pure offline commands must propagate nonzero process status so Compose
	// service_completed_successfully and scripts can trust failure.
	if isPureOfflineCLI(app, os.Args[1:]) {
		if err := app.RootCmd.Execute(); err != nil {
			os.Exit(1)
		}
		return
	}

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

type anonymousImageConfig struct {
	serverRecordID  string
	backendServerID string
	configured      bool
}

func anonymousImageConfigFromEnv() (anonymousImageConfig, error) {
	recordID, hasRecordID := os.LookupEnv("GATEWAY_ANONYMOUS_IMAGE_SERVER_RECORD_ID")
	backendID, hasBackendID := os.LookupEnv("GATEWAY_ANONYMOUS_IMAGE_BACKEND_SERVER_ID")
	return anonymousImageConfigFromValues(recordID, hasRecordID, backendID, hasBackendID)
}

func anonymousImageConfigFromValues(recordID string, hasRecordID bool, backendID string, hasBackendID bool) (anonymousImageConfig, error) {
	if !hasRecordID && !hasBackendID {
		return anonymousImageConfig{}, nil
	}
	if !hasRecordID || !hasBackendID || strings.TrimSpace(recordID) == "" || strings.TrimSpace(backendID) == "" {
		return anonymousImageConfig{}, fmt.Errorf("GATEWAY_ANONYMOUS_IMAGE_SERVER_RECORD_ID and GATEWAY_ANONYMOUS_IMAGE_BACKEND_SERVER_ID must both be set to non-empty values")
	}
	return anonymousImageConfig{serverRecordID: strings.TrimSpace(recordID), backendServerID: strings.TrimSpace(backendID), configured: true}, nil
}

type anonymousImageNamespaceValidator interface {
	ValidateAnonymousImageNamespace(context.Context) error
}

func validateAnonymousImageStartup(validator anonymousImageNamespaceValidator) (bool, error) {
	err := validator.ValidateAnonymousImageNamespace(context.Background())
	if err == nil {
		return false, nil
	}
	if gateway.IsAnonymousImageNamespaceTransient(err) {
		return true, nil
	}
	return false, err
}

func startAnonymousImageNamespace(validator anonymousImageNamespaceValidator, mount func()) (bool, error) {
	transient, err := validateAnonymousImageStartup(validator)
	if err != nil {
		return false, err
	}
	mount()
	return transient, nil
}

// isPureOfflineCLI reports whether args select a command that must not depend on
// PocketBase bootstrap and must return a real process exit code. Currently:
// `version` and the entire `web` subtree (init|install|status).
func isPureOfflineCLI(app *pocketbase.PocketBase, args []string) bool {
	cmd, _, err := app.RootCmd.Find(args)
	if err != nil || cmd == nil {
		return false
	}
	// Walk to the top-level command under RootCmd.
	for cmd.HasParent() && cmd.Parent() != app.RootCmd {
		cmd = cmd.Parent()
	}
	switch cmd.Name() {
	case "web", "version":
		return true
	default:
		return false
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

func registerMappingSessionRevocation(app core.App) {
	app.OnRecordUpdateExecute("user_mappings").BindFunc(func(e *core.RecordEvent) error {
		original, err := e.App.FindRecordById("user_mappings", e.Record.Id)
		if err != nil {
			return err
		}
		oldGatewayUserID := relationID(original, "gateway_user")
		newGatewayUserID := relationID(e.Record, "gateway_user")
		backendChanged := relationID(original, "backend_account") != relationID(e.Record, "backend_account")
		gatewayUserChanged := oldGatewayUserID != newGatewayUserID
		disabled := original.GetBool("enabled") && !e.Record.GetBool("enabled")
		if err := e.Next(); err != nil {
			return err
		}
		if !backendChanged && !gatewayUserChanged && !disabled {
			return nil
		}
		affected := []string{newGatewayUserID}
		if gatewayUserChanged {
			affected = append(affected, oldGatewayUserID)
		}
		for _, gatewayUserID := range uniqueNonEmpty(affected) {
			if err := revokeActiveGatewaySessionsForUser(e.App, gatewayUserID, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
}

func relationID(record *core.Record, field string) string {
	if record == nil {
		return ""
	}
	if value := strings.TrimSpace(record.GetString(field)); value != "" {
		return value
	}
	values := record.GetStringSlice(field)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func revokeActiveGatewaySessionsForUser(app core.App, gatewayUserID string, revokedAt time.Time) error {
	result, err := app.DB().NewQuery("update gateway_sessions set revoked_at = {:revokedAt} where gateway_user = {:gatewayUserID} and (revoked_at is null or revoked_at = '')").Bind(map[string]any{"revokedAt": revokedAt.UTC(), "gatewayUserID": gatewayUserID}).Execute()
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count <= 0 {
		return nil
	}
	audit, err := app.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		return err
	}
	record := core.NewRecord(audit)
	record.Set("gateway_user", gatewayUserID)
	record.Set("event", "sessions_revoked")
	record.Set("message", "mapping changed; active sessions revoked")
	record.Set("method", "UPDATE")
	record.Set("path", "user_mappings")
	record.Set("status", 200)
	return app.Save(record)
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	unique := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
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
