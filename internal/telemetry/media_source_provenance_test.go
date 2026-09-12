package telemetry

import (
	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"testing"
	"time"
)

func TestMediaSourceProvenancePlaybackRemainsPinnedAndLegacyStaysUnknown(t *testing.T) {
	for _, initial := range []string{"catalog-a", ""} {
		t.Run("initial="+initial, func(t *testing.T) {
			r := New(nil)
			now := time.Now().UTC()
			r.now = func() time.Time { return now }
			ev := observe.Event{Kind: observe.KindPlayback, At: now, SessionID: "session", UserID: "user", ItemID: "item", SourceRef: initial, PlaybackEvent: observe.PlaybackPlaying}
			r.handle(ev)
			ev.SourceRef = "catalog-b"
			ev.PlaybackEvent = observe.PlaybackProgress
			ev.PositionTicks = 300000000
			r.handle(ev)
			items := r.ActivePlaybacks()
			if len(items) != 1 || items[0].SourceRef != initial || items[0].PositionTicks != 300000000 {
				t.Fatalf("progress changed source or lost progress: %+v", items)
			}
			ev.PlaybackEvent = observe.PlaybackStopped
			r.handle(ev)
			ev.PlaybackEvent = observe.PlaybackPlaying
			r.handle(ev)
			items = r.ActivePlaybacks()
			if len(items) != 1 || items[0].SourceRef != "catalog-b" {
				t.Fatalf("new playback failed to capture new source: %+v", items)
			}
		})
	}
}

func TestMediaSourceProvenanceTransferMeterRetainsCapturedIdentity(t *testing.T) {
	m := NewByteMeter()
	meta := TransferMeta{SessionID: "session", UserID: "user", ItemID: "movie", SourceRef: "catalog-a"}
	h := m.BeginTransfer(meta)
	defer h.End(nil)
	meta.SourceRef = "catalog-b"
	meta.ItemID = "different"
	items := m.ActiveTransfers()
	if len(items) != 1 || items[0].SourceRef != "catalog-a" || items[0].ItemID != "movie" {
		t.Fatalf("capture changed: %+v", items)
	}
	h.End(nil)
	legacy := m.BeginTransfer(TransferMeta{ItemID: "legacy"})
	defer legacy.End(nil)
	items = m.ActiveTransfers()
	if len(items) != 1 || items[0].SourceRef != "" {
		t.Fatalf("unknown source inferred: %+v", items)
	}
}
