package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/pflag"
)

func TestGatewayCLIProcess(t *testing.T) {
	if os.Getenv("GATEWAY_CLI_PROCESS") != "1" {
		return
	}

	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("GATEWAY_CLI_ARGS")), &args); err != nil {
		os.Exit(2)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	os.Exit(runGateway(args))
}

func TestClassifiesDirectExecution(t *testing.T) {
	app := newGatewayApp()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "setup_root", args: []string{"setup"}, want: true},
		{name: "setup_root_global_dir_before", args: []string{"--dir", "/tmp/pb", "setup"}, want: true},
		{name: "setup_root_global_dir_after", args: []string{"setup", "--dir", "/tmp/pb"}, want: true},
		{name: "setup_unknown_subcommand", args: []string{"setup", "unknown"}, want: true},
		{name: "setup_unknown_flag", args: []string{"setup", "--unknown"}, want: true},
		{name: "setup_user_extra_arg", args: []string{"setup", "user", "extra"}, want: true},
		{name: "setup_user_missing_flag_value", args: []string{"setup", "user", "--gateway-username"}, want: true},
		{name: "setup_user_unknown_short_flag", args: []string{"setup", "user", "-x"}, want: true},
		{name: "setup_upstream_unknown_subcommand", args: []string{"setup", "upstream", "unknown"}, want: true},
		{name: "setup_upstream_bare", args: []string{"setup", "upstream"}, want: true},
		{name: "version_extra_arg", args: []string{"version", "extra"}, want: true},
		{name: "version_unknown_flag", args: []string{"version", "--unknown"}, want: true},
		{name: "serve", args: []string{"serve"}, want: false},
		{name: "superuser", args: []string{"superuser", "create", "admin@example.test", "test-password"}, want: false},
		{name: "unknown", args: []string{"unknown"}, want: true},
		{name: "unknown_global_flag_then_command", args: []string{"--unknown", "setup"}, want: true},
		{name: "empty", args: nil, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifiesDirectExecution(app, tc.args); got != tc.want {
				t.Fatalf("args=%v got %v want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestMalformedDirectCLIExitStatus(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "setup_unknown_subcommand", args: []string{"setup", "unknown"}},
		{name: "setup_unknown_flag", args: []string{"setup", "--unknown"}},
		{name: "setup_user_extra_arg", args: []string{"setup", "user", "extra"}},
		{name: "setup_user_missing_flag_value", args: []string{"setup", "user", "--gateway-username"}},
		{name: "setup_user_unknown_short_flag", args: []string{"setup", "user", "-x"}},
		{name: "setup_upstream_unknown_subcommand", args: []string{"setup", "upstream", "unknown"}},
		{name: "version_extra_arg", args: []string{"version", "extra"}},
		{name: "version_unknown_flag", args: []string{"version", "--unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertGatewayCLIExitCode(t, 1, tc.args...)
		})
	}
}

func TestRetiredCommandDoesNotBootstrapOrMutate(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	args := []string{"--dir", dataDir, "retired-command"}
	assertGatewayCLIExitCode(t, 1, args...)
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("retired invocation bootstrapped data dir: %v", err)
	}
}

func TestRetiredWebCommandsDoNotBootstrap(t *testing.T) {
	// Serve-only 0.8: web install/catalog CLI is retired. Unknown/retired web
	// commands must exit nonzero without creating the PocketBase data dir.
	cases := []struct {
		name string
		args []string
	}{
		{name: "web", args: []string{"web"}},
		{name: "web_init", args: []string{"web", "init"}},
		{name: "web_install", args: []string{"web", "install"}},
		{name: "web_status", args: []string{"web", "status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			args := append([]string{"--dir", dataDir}, tc.args...)
			assertGatewayCLIExitCode(t, 1, args...)
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("args=%v bootstrapped data dir: %v", args, err)
			}
		})
	}
}

func TestSuperuserCreateBootstrapsCanonicalSchema(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	email := "admin@example.test"
	assertGatewayCLIExitCode(t, 0, "--dir", dataDir, "superuser", "create", email, "test-password")

	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: dataDir})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap %s: %v", dataDir, err)
	}
	defer func() { _ = app.ResetBootstrapState() }()
	for _, name := range []string{"users", "gateway_sessions", "gateway_session_profiles", "audit_logs", "playback_events", "user_item_data", "item_child_counts", "display_preferences", "path_policies", "upstream_sources", "upstream_endpoints"} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("missing canonical collection %q: %v", name, err)
		}
	}
	if _, err := app.FindFirstRecordByData("_superusers", "email", email); err != nil {
		t.Fatalf("superuser %q missing: %v", email, err)
	}
}

func TestSuperuserErrorsBootstrapAndReturnNonzero(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	assertGatewayCLIExitCode(t, 1, "--dir", dataDir, "superuser", "unknown")
	assertGatewayCLIExitCode(t, 1, "--dir", dataDir, "superuser", "create")
	assertGatewayCLIExitCode(t, 1, "--dir", dataDir, "superuser", "create", "invalid", "test-password")
	assertGatewayCLIExitCode(t, 0, "--dir", dataDir, "superuser", "create", "admin@example.test", "test-password")
	assertGatewayCLIExitCode(t, 1, "--dir", dataDir, "superuser", "create", "admin@example.test", "test-password")
}

func TestRootHelpAndVersionDoNotBootstrap(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"version"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			output, err := gatewayCLIOutput(t, append([]string{"--dir", dataDir}, args...)...)
			if err != nil {
				t.Fatalf("args=%v: %v\n%s", args, err, output)
			}
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("args=%v bootstrapped data dir: %v", args, err)
			}
		})
	}
}

func TestHelpAndVersionRequestsDoNotBootstrap(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"root_long_help", []string{"--help"}, "Usage:"},
		{"root_short_help", []string{"-h"}, "Usage:"},
		{"root_long_version", []string{"--version"}, ""},
		{"root_short_version", []string{"-v"}, ""},
		{"setup_help", []string{"setup", "--help"}, "Configure gateway upstreams and users"},
		{"version_help", []string{"version", "--help"}, "Print gateway build version metadata"},
		{"serve_help", []string{"serve", "--help"}, "Starts the web server"},
		{"superuser_help", []string{"superuser", "--help"}, "Manage superusers"},
		{"superuser_nested_help", []string{"superuser", "create", "--help"}, "Creates a new superuser"},
		{"setup_nested_help", []string{"setup", "upstream", "--help"}, "Prepare singleton upstream configuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			output, err := gatewayCLIOutput(t, append([]string{"--dir", dataDir}, tc.args...)...)
			if err != nil {
				t.Fatalf("args=%v: %v\n%s", tc.args, err, output)
			}
			if len(output) == 0 || (tc.want != "" && !strings.Contains(string(output), tc.want)) {
				t.Fatalf("args=%v output=%q", tc.args, output)
			}
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("args=%v bootstrapped data dir: %v", tc.args, err)
			}
		})
	}
}

func TestCLIAppRegistersSystemCommandsExactlyOnce(t *testing.T) {
	app := newCLIApp()
	counts := map[string]int{}
	for _, command := range app.RootCmd.Commands() {
		counts[command.Name()]++
	}
	for _, name := range []string{"serve", "superuser"} {
		if counts[name] != 1 {
			t.Fatalf("%s registrations = %d", name, counts[name])
		}
	}
}

func TestServeInvalidMediaBufferConfigExitsNonzeroBeforeListen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(mediaBufferEnabledEnv, "true")
	t.Setenv(mediaBufferBudgetEnv, "")
	output, err := gatewayCLIOutput(t, "--dir", t.TempDir(), "serve", "--http="+address)
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("exit error=%v output=%s", err, output)
	}
	if !strings.Contains(string(output), mediaBufferBudgetEnv) || !strings.Contains(string(output), "explicit value is empty") {
		t.Fatalf("output=%q", output)
	}
	connection, dialErr := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatalf("invalid startup opened listener %s", address)
	}
}

func TestInvalidMediaBufferEnvDoesNotAffectUnrelatedCommands(t *testing.T) {
	t.Setenv(mediaBufferEnabledEnv, "true")
	t.Setenv(mediaBufferBudgetEnv, "")
	for _, args := range [][]string{{"setup", "--help"}, {"superuser", "--help"}, {"version"}} {
		output, err := gatewayCLIOutput(t, args...)
		if err != nil {
			t.Fatalf("args=%v error=%v output=%s", args, err, output)
		}
	}
}

func TestTerminateAndResetRunsHook(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	app := newCLIApp()
	terminated := 0
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		terminated++
		return e.Next()
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := terminateAndReset(app); err != nil {
		t.Fatal(err)
	}
	if terminated != 1 || app.IsBootstrapped() {
		t.Fatalf("terminated=%d bootstrapped=%v", terminated, app.IsBootstrapped())
	}
}

type lifecycleCloseSpy struct{ closes int }

func (s *lifecycleCloseSpy) Close()                                                { s.closes++ }
func (s *lifecycleCloseSpy) ValidateAnonymousImageNamespace(context.Context) error { return nil }
func (s *lifecycleCloseSpy) RefreshUpstreamServerInfo(context.Context) error       { return nil }

func TestGatewayServerLifecycleReplacesAndTerminatesExactlyOnce(t *testing.T) {
	app := pocketbase.New()
	lifecycle := &gatewayServerLifecycle{}
	bindGatewayLifecycle(app, lifecycle)
	first := &lifecycleCloseSpy{}
	second := &lifecycleCloseSpy{}
	lifecycle.Replace(first)
	lifecycle.Replace(second)
	if first.closes != 1 || second.closes != 0 {
		t.Fatalf("replace closes=%d/%d", first.closes, second.closes)
	}
	event := &core.TerminateEvent{App: app}
	if err := app.OnTerminate().Trigger(event, func(*core.TerminateEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.OnTerminate().Trigger(event, func(*core.TerminateEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if first.closes != 1 || second.closes != 1 {
		t.Fatalf("terminate closes=%d/%d", first.closes, second.closes)
	}
}

func TestTerminationFailureResetsAndFailsRun(t *testing.T) {
	original := newCLIAppForRun
	defer func() { newCLIAppForRun = original }()
	var execution *pocketbase.PocketBase
	newCLIAppForRun = func() *pocketbase.PocketBase {
		app := newCLIApp()
		app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
			return errors.New("terminate failure")
		})
		execution = app
		return app
	}
	if code := runGateway([]string{"version"}); code != 1 {
		t.Fatalf("runGateway exit code = %d", code)
	}
	if execution == nil || execution.IsBootstrapped() {
		t.Fatalf("execution app reset = %#v", execution)
	}
}

func TestPersistentFlagsClassifyAndPreserveDirectCommands(t *testing.T) {
	flags := gatewayPersistentFlags(t)
	if len(flags) == 0 {
		t.Fatal("PocketBase root command has no persistent flags")
	}
	for _, flag := range flags {
		t.Run(flag.Name, func(t *testing.T) {
			value := gatewayFlagValue(flag, t.TempDir())
			for _, form := range [][]string{{"--" + flag.Name, value}, {"--" + flag.Name + "=" + value}} {
				for _, command := range [][]string{{"serve"}, {"superuser", "create", "admin@example.test", "test-password"}, {"setup"}, {"unknown-command"}} {
					args := append(append([]string{}, form...), command...)
					wantDirect := command[0] != "serve" && command[0] != "superuser"
					if flag.Value.Type() == "bool" && len(form) == 2 {
						wantDirect = true
					}
					if got := classifiesDirectExecution(newGatewayApp(), args); got != wantDirect {
						t.Fatalf("args=%v direct=%v want=%v", args, got, wantDirect)
					}
				}
				if got := classifiesDirectExecution(newGatewayApp(), append([]string{"setup"}, form...)); !got {
					t.Fatalf("global flag after direct command classified as Start: %v", form)
				}
				if got := classifiesDirectExecution(newGatewayApp(), []string{"--" + flag.Name}); !got {
					t.Fatalf("missing persistent flag value classified as Start: %s", flag.Name)
				}
			}
		})
	}
}

func TestPersistentFlagsProcessBehavior(t *testing.T) {
	t.Setenv("GATEWAY_CLI_ENCRYPTION", "01234567890123456789012345678901")
	for _, flag := range gatewayPersistentFlags(t) {
		t.Run(flag.Name, func(t *testing.T) {
			for _, equals := range []bool{false, true} {
				t.Run(map[bool]string{false: "separate", true: "equals"}[equals], func(t *testing.T) {
					dataDir := filepath.Join(t.TempDir(), "data")
					flagArgs := gatewayFlagArgs(flag, gatewayFlagValue(flag, dataDir), equals)
					if flag.Name != "dir" {
						flagArgs = append(flagArgs, "--dir", dataDir)
					}
					if flag.Value.Type() == "bool" && !equals {
						assertGatewayCLIExitCode(t, 1, append(flagArgs, "superuser", "create", "admin@example.test", "test-password")...)
						if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
							t.Fatalf("malformed boolean flag bootstrapped data dir: %v", err)
						}
						return
					}
					assertGatewayCLIExitCode(t, 0, append(flagArgs, "superuser", "create", "admin@example.test", "test-password")...)
					app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: dataDir, DefaultEncryptionEnv: "GATEWAY_CLI_ENCRYPTION"})
					if app.EncryptionEnv() != "GATEWAY_CLI_ENCRYPTION" {
						t.Fatalf("encryption env = %q", app.EncryptionEnv())
					}
					if err := app.Bootstrap(); err != nil {
						t.Fatal(err)
					}
					if _, err := app.FindFirstRecordByData("_superusers", "email", "admin@example.test"); err != nil {
						t.Fatalf("superuser missing: %v", err)
					}
					_ = app.ResetBootstrapState()
				})
			}
		})
	}

	for _, flag := range gatewayPersistentFlags(t) {
		t.Run("direct_"+flag.Name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			args := gatewayFlagArgs(flag, gatewayFlagValue(flag, dataDir), false)
			if flag.Name != "dir" {
				args = append(args, "--dir", dataDir)
			}
			output, err := gatewayCLIOutput(t, append(args, "setup")...)
			if flag.Value.Type() == "bool" {
				if err == nil {
					t.Fatalf("malformed boolean setup succeeded: %s", flag.Name)
				}
			} else if err != nil {
				t.Fatalf("setup with %s: %v\n%s", flag.Name, err, output)
			}
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("direct setup bootstrapped data dir for %s: %v", flag.Name, err)
			}
			assertGatewayCLIExitCode(t, 1, append(args, "unknown-command")...)
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("unknown command bootstrapped data dir for %s: %v", flag.Name, err)
			}
			afterDir := filepath.Join(t.TempDir(), "after")
			afterArgs := append([]string{"setup"}, gatewayFlagArgs(flag, gatewayFlagValue(flag, afterDir), true)...)
			if flag.Name != "dir" {
				afterArgs = append(afterArgs, "--dir", afterDir)
			}
			if output, err := gatewayCLIOutput(t, afterArgs...); err != nil {
				t.Fatalf("setup after %s: %v\n%s", flag.Name, err, output)
			}
			if _, err := os.Stat(afterDir); !os.IsNotExist(err) {
				t.Fatalf("direct setup after flag bootstrapped data dir for %s: %v", flag.Name, err)
			}
			if flag.Value.Type() != "bool" {
				missingDir := filepath.Join(t.TempDir(), "missing")
				assertGatewayCLIExitCode(t, 1, "--dir", missingDir, "--"+flag.Name)
				if _, err := os.Stat(missingDir); !os.IsNotExist(err) {
					t.Fatalf("missing flag value bootstrapped data dir for %s: %v", flag.Name, err)
				}
			}
		})
	}
}

func gatewayPersistentFlags(t *testing.T) []*pflag.Flag {
	t.Helper()
	var flags []*pflag.Flag
	newGatewayApp().RootCmd.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, flag)
	})
	return flags
}

func classifiesDirectExecution(_ *pocketbase.PocketBase, args []string) bool {
	mode, err := validateCommand(newCLIApp(), args)
	return err != nil || mode == commandDirect
}

func gatewayFlagValue(flag *pflag.Flag, dataDir string) string {
	if flag.Name == "dir" {
		return dataDir
	}
	switch flag.Value.Type() {
	case "bool":
		return "true"
	case "int", "int32", "int64", "uint", "uint32", "uint64":
		return "1"
	default:
		return "GATEWAY_CLI_ENCRYPTION"
	}
}

func gatewayFlagArgs(flag *pflag.Flag, value string, equals bool) []string {
	if equals {
		return []string{"--" + flag.Name + "=" + value}
	}
	return []string{"--" + flag.Name, value}
}

func TestSetupBareShowsHelpWithoutBootstrap(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	output, err := gatewayCLIOutput(t, "--dir", dataDir, "setup")
	if err != nil {
		t.Fatalf("bare setup: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Configure gateway upstreams and users") {
		t.Fatalf("setup help missing from output:\n%s", output)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("bare setup bootstrapped data dir: %v", err)
	}
}

func TestSetupUpstreamBareShowsHelp(t *testing.T) {
	output, err := gatewayCLIOutput(t, "setup", "upstream")
	if err != nil {
		t.Fatalf("exit error: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Prepare singleton upstream configuration") {
		t.Fatalf("upstream help missing from output:\n%s", output)
	}
}

func TestSetupUserUsesRequestedDataDir(t *testing.T) {
	cases := []struct {
		name   string
		before bool
	}{
		{name: "global_dir_before_setup", before: true},
		{name: "global_dir_after_setup", before: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestedDir := filepath.Join(t.TempDir(), "requested")
			alternateDir := filepath.Join(t.TempDir(), "alternate")
			username := "user-" + tc.name
			args := []string{"setup", "user", "--gateway-username", username, "--gateway-password", "test-password", "--synthetic-user-id", "synthetic-" + tc.name}
			if tc.before {
				args = append([]string{"--dir", requestedDir}, args...)
			} else {
				args = append(args, "--dir", requestedDir)
			}

			assertGatewayCLIExitCode(t, 0, args...)
			assertGatewayUserInDataDir(t, requestedDir, username, true)
			assertGatewayUserInDataDir(t, alternateDir, username, false)
		})
	}
}

func assertGatewayCLIExitCode(t *testing.T, wantCode int, args ...string) {
	t.Helper()
	output, err := gatewayCLIOutput(t, args...)
	if wantCode == 0 {
		if err != nil {
			t.Fatalf("exit error: %v\n%s", err, output)
		}
		return
	}
	if err == nil {
		t.Fatalf("exit code = 0, want %d\n%s", wantCode, output)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != wantCode {
		t.Fatalf("exit error = %v, want code %d\n%s", err, wantCode, output)
	}
}

func gatewayCLIOutput(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGatewayCLIProcess$", "--")
	cmd.Env = append(os.Environ(), "GATEWAY_CLI_PROCESS=1", "GATEWAY_CLI_ARGS="+string(encoded), "GATEWAY_CLI_ENCRYPTION=01234567890123456789012345678901")
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("gateway CLI timed out after 10s: args=%v\n%s", args, output)
	}
	return output, err
}

func assertGatewayUserInDataDir(t *testing.T, dataDir, username string, want bool) {
	t.Helper()
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: dataDir})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap %s: %v", dataDir, err)
	}
	defer func() { _ = app.ResetBootstrapState() }()

	if want {
		if _, err := app.FindCollectionByNameOrId("users"); err != nil {
			t.Fatalf("schema was not initialized in %s: %v", dataDir, err)
		}
	}
	_, err := app.FindFirstRecordByData("users", "username", username)
	if want && err != nil {
		t.Fatalf("gateway user %q missing from %s: %v", username, dataDir, err)
	}
	if !want && err == nil {
		t.Fatalf("gateway user %q unexpectedly found in %s", username, dataDir)
	}
}
