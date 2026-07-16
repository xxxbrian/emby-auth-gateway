package pbsetup

import (
	"bytes"
	"testing"
)

func TestSetupCommandIsGroupOnly(t *testing.T) {
	cmd := NewCommand(newTestApp(t))
	output := &bytes.Buffer{}
	cmd.SetOut(output)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare setup: %v", err)
	}
	if output.Len() == 0 {
		t.Fatal("bare setup did not render help")
	}
	for _, name := range []string{"gateway-username", "gateway-password", "synthetic-user-id", "emby-server-name", "emby-url", "backend-account-name", "backend-username", "backend-password", "backend-user-agent", "backend-authorization-client", "backend-authorization-device", "backend-authorization-version"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Fatalf("retired root flag %q remains", name)
		}
	}
}
