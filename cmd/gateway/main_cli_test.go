package main

import (
	"testing"

	"github.com/pocketbase/pocketbase"
)

func TestIsPureOfflineCLI(t *testing.T) {
	app := pocketbase.New()
	app.RootCmd.AddCommand(newVersionCommand())
	app.RootCmd.AddCommand(newWebCommand())

	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "web_init", args: []string{"web", "init", "--catalog-id", "x"}, want: true},
		{name: "web_init_global_dir", args: []string{"--dir", "/tmp/pb", "web", "init"}, want: true},
		{name: "web_status", args: []string{"web", "status"}, want: true},
		{name: "version", args: []string{"version"}, want: true},
		{name: "serve", args: []string{"serve"}, want: false},
		{name: "empty", args: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPureOfflineCLI(app, tc.args)
			if got != tc.want {
				t.Fatalf("args=%v got %v want %v", tc.args, got, tc.want)
			}
		})
	}
}
