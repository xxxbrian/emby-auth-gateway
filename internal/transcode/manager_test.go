package transcode

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerDemandSeekAndIsolation(t *testing.T) {
	path := ffmpegFixture(t, "mkv")
	source := fileSource(t, path, "mkv", 40*TicksPerSecond)
	plan, err := Choose(source.Media, Request{Profile: testProfile("2")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), Config{CacheDir: t.TempDir(), Workers: 2, CacheBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id, err := m.Create(Identity{Owner: "owner", ItemID: "item"}, source, *plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.HasActiveWork() {
		t.Fatal("metadata registered work")
	}
	if !m.OwnsItem("owner", id, "item") || m.OwnsItem("other", id, "item") || m.OwnsItem("owner", id, "other") {
		t.Fatal("incorrect ownership")
	}
	manifest, err := m.Manifest(context.Background(), "owner", id)
	if err != nil || !strings.Contains(manifest, "#EXT-X-ENDLIST") {
		t.Fatalf("manifest: %v %s", err, manifest)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, n, e := m.Open(context.Background(), "owner", id, 0)
			if e != nil {
				t.Error(e)
				return
			}
			defer f.Close()
			count, e := io.Copy(io.Discard, f)
			if e != nil || count != n {
				t.Errorf("incomplete media: %d/%d %v", count, n, e)
			}
		}()
	}
	wg.Wait()
	for _, index := range []int{7, 1, 5} {
		f, n, e := m.Open(context.Background(), "owner", id, index)
		if e != nil {
			t.Fatalf("seek %d: %v", index, e)
		}
		if n == 0 {
			t.Fatal("empty segment")
		}
		_ = f.Close()
	}
	if _, _, err = m.Open(context.Background(), "other", id, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-user segment available")
	}
	if m.End("other", id) {
		t.Fatal("cross-user stop")
	}
	m.StopEncoding("owner", id)
	f, _, err := m.Open(context.Background(), "owner", id, 0)
	if err != nil {
		t.Fatal("stop encoding invalidated cached playback", err)
	}
	_ = f.Close()
	next, err := m.Create(Identity{Owner: "owner", ItemID: "item"}, source, *plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.LinkReplacement("owner", id, next)
	m.StopEncoding("owner", id)
	if m.OwnsItem("owner", id, "item") || !m.OwnsItem("owner", next, "item") {
		t.Fatal("replacement ownership did not transition")
	}
	m.mu.Lock()
	samePlayback := m.jobs[next].playbackID == id
	m.mu.Unlock()
	if !samePlayback {
		t.Fatal("audio change created another logical playback")
	}
	id = next
	if !m.End("owner", id) {
		t.Fatal("stop did not find playback")
	}
	if _, _, err = m.Open(context.Background(), "owner", id, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("ended playback remained available")
	}
}

func TestManagerCanceledWaiterAndCacheRecovery(t *testing.T) {
	path := ffmpegFixture(t, "mkv")
	source := fileSource(t, path, "mkv", 40*TicksPerSecond)
	plan, _ := Choose(source.Media, Request{Profile: testProfile("6")})
	m, err := New(context.Background(), Config{CacheDir: t.TempDir(), Workers: 1, CacheBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id, err := m.Create(Identity{Owner: "owner", ItemID: "item"}, source, *plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = m.Open(ctx, "owner", id, 0); err == nil {
		t.Fatal("canceled request succeeded")
	}
	f, _, err := m.Open(context.Background(), "owner", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// Eviction affects physical artifacts, not the seekable VOD timeline.
	m.cache.dropPrefix(id + "/")
	f, _, err = m.Open(context.Background(), "owner", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	m.publishObservation()
	observation := m.Snapshot()
	if len(observation.Jobs) != 1 || observation.Jobs[0].AudioOutputChannels != 6 {
		t.Fatalf("observation: %+v", observation)
	}
	m.NotePlayback("owner", id, 12*TicksPerSecond, nil)
	old := observation.Jobs[0].PositionTicks
	m.publishObservation()
	if old != nil {
		t.Fatal("old observation mutated")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker shutdown did not join")
	}
}
