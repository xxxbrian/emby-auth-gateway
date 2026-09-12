package adminmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readerStub struct {
	source atomic.Value
	read   func(context.Context, string, []string, bool) ([]byte, int, error)
	image  func(context.Context, string, string, string, string) ([]byte, string, int, error)
}

func stub() *readerStub { r := &readerStub{}; r.source.Store("source-a"); return r }
func (r *readerStub) AdminMediaSourceRef(ctx context.Context) (string, error) {
	return r.source.Load().(string), nil
}
func (r *readerStub) AdminMediaItems(ctx context.Context, source string, ids []string, detail bool) ([]byte, int, error) {
	return r.read(ctx, source, ids, detail)
}
func (r *readerStub) AdminMediaImage(ctx context.Context, source, id, kind, size string) ([]byte, string, int, error) {
	return r.image(ctx, source, id, kind, size)
}

func TestMetadataProjectionPartialMissingAndParentImage(t *testing.T) {
	r := stub()
	var calls int
	r.read = func(_ context.Context, _ string, ids []string, detail bool) ([]byte, int, error) {
		calls++
		if len(ids) != 2 {
			t.Fatalf("deduplicated IDs: %v", ids)
		}
		return []byte(`{"Items":[{"Id":"episode","Name":"Arrival","Type":"Episode","SeriesName":"A Show","ParentIndexNumber":0,"IndexNumber":2,"RunTimeTicks":3000000000,"SeriesId":"series","SeriesPrimaryImageTag":"tag","UserData":{"IsFavorite":true,"PlaybackPositionTicks":123},"Path":"/private/movie.mkv","MediaSources":[{"Path":"https://secret?api_key=token"}]}]}`), 200, nil
	}
	s := New(r)
	items, err := s.Items(context.Background(), "source-a", []string{"episode", "missing", "episode"}, false)
	if err != nil || len(items) != 2 || items[0].Status != "available" || items[1].Status != "missing" || items[0].SeasonNumber == nil || *items[0].SeasonNumber != 0 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if !strings.Contains(items[0].Image, "/items/series/images/Primary?") {
		t.Fatalf("parent image=%s", items[0].Image)
	}
	if items[0].SeriesID != "series" {
		t.Fatalf("canonical series reference=%q", items[0].SeriesID)
	}
	encoded, _ := json.Marshal(items)
	for _, secret := range []string{"UserData", "IsFavorite", "PlaybackPosition", "private", "MediaSources", "api_key"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	_, _ = s.Items(context.Background(), "source-a", []string{"episode", "missing"}, false)
	if calls != 1 {
		t.Fatalf("cache reads=%d", calls)
	}
}

func TestMetadataOverlapCoalescesIDs(t *testing.T) {
	r := stub()
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	counts := map[string]int{}
	r.read = func(ctx context.Context, _ string, ids []string, _ bool) ([]byte, int, error) {
		mu.Lock()
		for _, id := range ids {
			counts[id]++
		}
		mu.Unlock()
		if ids[0] == "a" {
			close(entered)
			<-release
		}
		items := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			items = append(items, map[string]string{"Id": id, "Name": id})
		}
		data, _ := json.Marshal(map[string]any{"Items": items})
		return data, 200, nil
	}
	s := New(r)
	done := make(chan []Item, 2)
	go func() {
		items, _ := s.Items(context.Background(), "source-a", []string{"a", "b"}, false)
		done <- items
	}()
	<-entered
	go func() {
		items, _ := s.Items(context.Background(), "source-a", []string{"b", "c"}, false)
		done <- items
	}()
	// Wait until the second batch has claimed c; it then awaits the first b.
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		claimed := counts["c"] == 1
		mu.Unlock()
		if claimed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second batch did not start")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	for range 2 {
		for _, item := range <-done {
			if item.Status != "available" {
				t.Fatalf("item=%+v", item)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != 1 {
			t.Fatalf("count %s=%d", id, counts[id])
		}
	}
}

func TestMetadataSourceChangeAndRefreshFailure(t *testing.T) {
	r := stub()
	var calls int
	failure := false
	r.read = func(_ context.Context, _ string, _ []string, _ bool) ([]byte, int, error) {
		calls++
		if failure {
			return nil, 503, errors.New("timeout")
		}
		return []byte(`{"Items":[{"Id":"a","Name":"Original"}]}`), 200, nil
	}
	s := New(r)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	items, _ := s.Items(context.Background(), "source-a", []string{"a"}, false)
	if items[0].Status != "available" {
		t.Fatal(items)
	}
	now = now.Add(11 * time.Minute)
	failure = true
	items, _ = s.Items(context.Background(), "source-a", []string{"a"}, false)
	if !items[0].Stale || items[0].Name != "Original" || items[0].Status != "available" {
		t.Fatalf("stale=%+v", items)
	}
	r.source.Store("source-b")
	items, _ = s.Items(context.Background(), "source-a", []string{"a"}, false)
	if items[0].Status != "source_changed" || items[0].Name != "" || calls != 2 {
		t.Fatalf("changed=%+v calls=%d", items, calls)
	}
	items, _ = s.Items(context.Background(), "", []string{"a"}, false)
	if items[0].Status != "source_changed" || calls != 2 {
		t.Fatal("unverified source was resolved")
	}
}

func TestMetadataChangeWhileRequestInFlight(t *testing.T) {
	r := stub()
	r.read = func(_ context.Context, _ string, _ []string, _ bool) ([]byte, int, error) {
		r.source.Store("source-b")
		return []byte(`{"Items":[{"Id":"a","Name":"Old"}]}`), 200, nil
	}
	items, _ := New(r).Items(context.Background(), "source-a", []string{"a"}, false)
	if items[0].Status != "source_changed" || items[0].Name != "" {
		t.Fatalf("items=%+v", items)
	}
}

func TestMetadataDetailWhitelistsTracksAndLimitsStrings(t *testing.T) {
	r := stub()
	r.read = func(_ context.Context, _ string, _ []string, detail bool) ([]byte, int, error) {
		if !detail {
			t.Fatal("detail not requested")
		}
		data, _ := json.Marshal(map[string]any{"Id": "a", "Overview": strings.Repeat("界", 9000), "MediaStreams": []any{map[string]any{"Type": "Audio", "Codec": "aac", "Language": "en", "DisplayTitle": "Stereo", "Channels": 2, "Path": "secret"}, map[string]any{"Type": "Attachment", "Path": "secret"}}})
		return data, 200, nil
	}
	items, _ := New(r).Items(context.Background(), "source-a", []string{"a"}, true)
	if len([]rune(items[0].Overview)) != 8000 || len(items[0].MediaStreams) != 1 || items[0].MediaStreams[0].Channels != 2 {
		t.Fatalf("detail=%+v", items[0].MediaStreams)
	}
}

func TestMetadataCacheIsBoundedAndBadRequestsDoNotRead(t *testing.T) {
	r := stub()
	r.read = func(_ context.Context, _ string, ids []string, _ bool) ([]byte, int, error) {
		rows := make([]map[string]string, len(ids))
		for i, id := range ids {
			rows[i] = map[string]string{"Id": id, "Name": strings.Repeat("x", 500)}
		}
		data, _ := json.Marshal(map[string]any{"Items": rows})
		return data, 200, nil
	}
	s := New(r)
	for batch := range 22 {
		ids := make([]string, 50)
		for i := range ids {
			ids[i] = fmt.Sprintf("item-%d", batch*50+i)
		}
		if _, err := s.Items(context.Background(), "source-a", ids, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.cache) > maxCacheEntries || s.cacheBytes > maxCacheBytes {
		t.Fatalf("cache entries=%d bytes=%d", len(s.cache), s.cacheBytes)
	}
	for _, ids := range [][]string{nil, {"../a"}, make([]string, 51)} {
		if _, err := s.Items(context.Background(), "source-a", ids, false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid IDs accepted: %v", ids)
		}
	}
	if _, err := s.Items(context.Background(), strings.Repeat("x", 129), []string{"a"}, false); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded source reference accepted")
	}
}

func TestImageOnlyPassiveRasterAndSeparateBudget(t *testing.T) {
	r := stub()
	var calls int
	r.image = func(_ context.Context, _ string, _ string, _ string, _ string) ([]byte, string, int, error) {
		calls++
		return []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>bad()</script></svg>`), "image/jpeg", 200, nil
	}
	s := New(r)
	if _, _, status := s.Image(context.Background(), "source-a", "a", "Primary", "small"); status != 502 {
		t.Fatalf("active image status=%d", status)
	}
	r.image = func(_ context.Context, _ string, _ string, _ string, _ string) ([]byte, string, int, error) {
		calls++
		return append([]byte{0xff, 0xd8, 0xff}, make([]byte, 100)...), "text/html", 200, nil
	}
	data, kind, status := s.Image(context.Background(), "source-a", "a", "Primary", "small")
	if status != 200 || kind != "image/jpeg" || len(data) != 103 {
		t.Fatalf("image=%s/%d", kind, status)
	}
	_, _, _ = s.Image(context.Background(), "source-a", "a", "Primary", "small")
	if calls != 2 {
		t.Fatalf("image cache calls=%d", calls)
	}
	r.source.Store("source-b")
	if _, _, status := s.Image(context.Background(), "source-a", "a", "Primary", "small"); status != 409 {
		t.Fatalf("old cached image status=%d", status)
	}
	if _, _, status := s.Image(context.Background(), "source-b", "../a", "Primary", "small"); status != 400 {
		t.Fatal("path traversal accepted")
	}
}

func TestMetadataConcurrencyIsBounded(t *testing.T) {
	r := stub()
	var active, peak atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	r.read = func(ctx context.Context, _ string, _ []string, _ bool) ([]byte, int, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []byte(`{"Items":[]}`), 200, nil
	}
	s := New(r)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Items(context.Background(), "source-a", []string{fmt.Sprintf("item-%d", i)}, false)
		}()
	}
	for range 4 {
		<-entered
	}
	close(release)
	// Drain completion notifications so later batches cannot block the stub.
	go func() {
		for range entered {
		}
	}()
	wg.Wait()
	close(entered)
	if peak.Load() > 4 {
		t.Fatalf("metadata concurrency=%d", peak.Load())
	}
}

func TestMetadataCancellationReleasesPendingReads(t *testing.T) {
	r := stub()
	r.read = func(ctx context.Context, _ string, _ []string, _ bool) ([]byte, int, error) {
		<-ctx.Done()
		return nil, 503, ctx.Err()
	}
	s := New(r)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	items, err := s.Items(ctx, "source-a", []string{"a"}, false)
	if err != nil || items[0].Status != "unavailable" {
		t.Fatalf("cancelled items=%+v err=%v", items, err)
	}
	if len(s.pending) != 0 || len(s.metadataSlots) != 0 {
		t.Fatal("cancelled metadata retained work")
	}
	r.read = func(context.Context, string, []string, bool) ([]byte, int, error) {
		return []byte(`{"Items":[{"Id":"b","Name":"Still available"}]}`), 200, nil
	}
	items, err = s.Items(context.Background(), "source-a", []string{"b"}, false)
	if err != nil || items[0].Status != "available" {
		t.Fatalf("request after cancel=%+v err=%v", items, err)
	}
}
