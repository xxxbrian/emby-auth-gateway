package gateway_test

import (
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

func TestNewMediaBufferExportedOpaqueController(t *testing.T) {
	controller, err := gateway.NewMediaBuffer(32 << 10)
	if err != nil || controller == nil {
		t.Fatalf("NewMediaBuffer() controller=%v error=%v", controller, err)
	}
	if _, err := gateway.NewMediaBuffer((32 << 10) + 1); err == nil {
		t.Fatal("NewMediaBuffer() accepted an unaligned budget")
	}
	config := gateway.Config{MediaBuffer: controller}
	if config.MediaBuffer != controller {
		t.Fatal("Config did not retain the opaque controller")
	}
}
