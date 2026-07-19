package gateway

import (
	"reflect"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestServerMediaBufferSnapshot(t *testing.T) {
	var nilServer *Server
	if got := nilServer.MediaBufferSnapshot(); got != (telemetry.MediaBufferStatus{}) {
		t.Fatalf("nil server snapshot=%+v", got)
	}
	disabled := NewServer(Config{}, NewMemoryStore())
	if got := disabled.MediaBufferSnapshot(); got != (telemetry.MediaBufferStatus{}) {
		t.Fatalf("disabled snapshot=%+v", got)
	}

	controller, err := NewMediaBuffer(2 * mediaBufferChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	enabled := NewServer(Config{MediaBuffer: controller}, NewMemoryStore())
	if got, want := enabled.MediaBufferSnapshot(), (telemetry.MediaBufferStatus{Enabled: true, HardBudgetBytes: 2 * mediaBufferChunkSize}); got != want {
		t.Fatalf("initial snapshot=%+v want %+v", got, want)
	}

	first := controller.controller.register()
	firstLeases := []mediaBufferLease{
		requireAcceptedMediaBufferLease(t, first),
		requireAcceptedMediaBufferLease(t, first),
	}
	second := controller.controller.register()
	controllerSnapshot := controller.controller.Snapshot()
	want := telemetry.MediaBufferStatus{
		Enabled:          controllerSnapshot.Enabled,
		HardBudgetBytes:  controllerSnapshot.HardBudget,
		AllocatedBytes:   controllerSnapshot.Allocated,
		OwnedBytes:       controllerSnapshot.Owned,
		FreeBytes:        controllerSnapshot.Free,
		ActiveRequests:   controllerSnapshot.ActiveRequests,
		BaseOnlyRequests: controllerSnapshot.BaseOnlyRequests,
		IndebtedRequests: controllerSnapshot.IndebtedRequests,
		RequestDebtBytes: controllerSnapshot.RequestDebtBytes,
	}
	if want.ActiveRequests != 2 || want.BaseOnlyRequests != 1 || want.IndebtedRequests != 1 || want.RequestDebtBytes != mediaBufferChunkSize {
		t.Fatalf("fixture aggregate=%+v", want)
	}
	if got := enabled.MediaBufferSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nonzero snapshot=%+v want %+v", got, want)
	}
	if err := first.releaseOptional(firstLeases[0]); err != nil {
		t.Fatal(err)
	}
	if err := first.releaseOptional(firstLeases[1]); err != nil {
		t.Fatal(err)
	}
	closeMediaBufferRequests(t, first, second)
}

func TestServerMediaBufferControllerSnapshotProvider(t *testing.T) {
	var nilServer *Server
	if got := nilServer.MediaBufferControllerSnapshot(); got != (telemetry.MediaBufferControllerSnapshot{}) {
		t.Fatalf("nil provider snapshot=%+v", got)
	}
	disabled := NewServer(Config{}, NewMemoryStore())
	if got := disabled.MediaBufferControllerSnapshot(); got.Available || got.Enabled {
		t.Fatalf("disabled provider snapshot=%+v", got)
	}

	controller, err := NewMediaBuffer(2 * mediaBufferChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	enabled := NewServer(Config{MediaBuffer: controller}, NewMemoryStore())
	request := controller.controller.register()
	got := enabled.MediaBufferControllerSnapshot()
	if !got.Available || !got.Enabled || got.HardBudgetBytes != 2*mediaBufferChunkSize || got.ActiveRequests != 1 || got.PrivateBaseBytes != mediaBufferChunkSize || got.UnallocatedOptionalBytes != 2*mediaBufferChunkSize {
		t.Fatalf("enabled provider snapshot=%+v", got)
	}
	closeMediaBufferRequests(t, request)
}
