package telemetry

import (
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
)

func TestSubtitleSnapshotNilAndProviderLifecycle(t *testing.T) {
	var missing *Registry
	missing.SetSubtitlesProvider(nil)
	if snapshot := missing.SubtitleSnapshot(); snapshot.Enabled || snapshot.Jobs == nil {
		t.Fatalf("nil registry: %+v", snapshot)
	}
	r := New(nil)
	if snapshot := r.SubtitleSnapshot(); snapshot.Enabled || snapshot.Jobs == nil || snapshot.BootID != r.BootID() {
		t.Fatalf("disabled snapshot: %+v", snapshot)
	}
	calls := 0
	r.SetSubtitlesProvider(func() subtitles.Snapshot {
		calls++
		return subtitles.Snapshot{Enabled: true, Workers: 1, Active: 1}
	})
	if calls != 0 {
		t.Fatal("installing observation provider invoked subtitle work")
	}
	if snapshot := r.SubtitleSnapshot(); !snapshot.Enabled || snapshot.Active != 1 || snapshot.Jobs == nil || snapshot.BootID != r.BootID() || calls != 1 {
		t.Fatalf("provider snapshot: %+v calls=%d", snapshot, calls)
	}
	r.SetSubtitlesProvider(nil)
	if r.SubtitleSnapshot().Enabled || calls != 1 {
		t.Fatal("removed provider still active")
	}
}
