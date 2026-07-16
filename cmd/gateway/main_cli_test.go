package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
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

func TestSelectsDirectExecution(t *testing.T) {
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
		{name: "setup_upstream_retired_subcommand", args: []string{"setup", "upstream", "import-legacy"}, want: true},
		{name: "setup_upstream_bare", args: []string{"setup", "upstream"}, want: true},
		{name: "web_unknown_subcommand", args: []string{"web", "unknown"}, want: true},
		{name: "web_unknown_flag", args: []string{"web", "init", "--unknown"}, want: true},
		{name: "web_missing_flag_value", args: []string{"web", "init", "--catalog-id"}, want: true},
		{name: "version_extra_arg", args: []string{"version", "extra"}, want: true},
		{name: "version_unknown_flag", args: []string{"version", "--unknown"}, want: true},
		{name: "serve", args: []string{"serve"}, want: false},
		{name: "migrate", args: []string{"migrate", "up"}, want: false},
		{name: "unknown", args: []string{"unknown"}, want: false},
		{name: "unknown_global_flag_then_command", args: []string{"--unknown", "setup"}, want: false},
		{name: "empty", args: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectsDirectExecution(app, tc.args); got != tc.want {
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
		{name: "setup_upstream_retired_subcommand", args: []string{"setup", "upstream", "import-legacy"}},
		{name: "web_unknown_subcommand", args: []string{"web", "unknown"}},
		{name: "web_unknown_flag", args: []string{"web", "init", "--unknown"}},
		{name: "web_missing_flag_value", args: []string{"web", "init", "--catalog-id"}},
		{name: "version_extra_arg", args: []string{"version", "extra"}},
		{name: "version_unknown_flag", args: []string{"version", "--unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertGatewayCLIExitCode(t, 1, tc.args...)
		})
	}
}

func TestRetiredSetupInvocationDoesNotBootstrapOrMutate(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	args := []string{"--dir", dataDir, "setup", "upstream", "import-legacy"}
	assertGatewayCLIExitCode(t, 1, args...)
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("retired invocation bootstrapped data dir: %v", err)
	}
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
	cmd.Env = append(os.Environ(), "GATEWAY_CLI_PROCESS=1", "GATEWAY_CLI_ARGS="+string(encoded))
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
			t.Fatalf("migrations did not run in %s: %v", dataDir, err)
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
