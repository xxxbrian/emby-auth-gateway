package subtitles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const vtt = "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nA subtitle\n"

func newManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.PrepareTimeout == 0 {
		cfg.PrepareTimeout = time.Second
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func input() Input {
	return Input{Owner: "owner", PlaybackID: "play", ItemID: "item", SourceID: "source", SourceKey: "source-key", SourceName: "Title", Size: 50 << 30, Container: "mkv", Tracks: []Track{{Index: 0, Codec: "subrip", Language: "eng", Name: "English"}}, Valid: func(context.Context) bool { return true }, Fetch: func(context.Context, Track) ([]byte, string, error) { return []byte(vtt), "vtt", nil }}
}

func selected(index int) Selection { return Selection{Playback: true, Probe: true, Index: &index} }

func readReady(t *testing.T, m *Manager, in Input, ready []Ready) string {
	t.Helper()
	if len(ready) != 1 {
		t.Fatalf("ready = %#v, snapshot=%#v", ready, m.Snapshot())
	}
	f, format, err := m.Open(context.Background(), in.Owner, in.ItemID, ready[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if format != "vtt" {
		t.Fatalf("format=%s", format)
	}
	if _, err = f.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadyOnlyAndNativeProjectionDoesNoWork(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	var calls, reads atomic.Int32
	in.Fetch = func(context.Context, Track) ([]byte, string, error) { calls.Add(1); return []byte(vtt), "vtt", nil }
	in.Open = func(context.Context, int64, int64) (io.ReadCloser, error) {
		reads.Add(1)
		return nil, errors.New("no raw reads")
	}
	if got := m.Prepare(context.Background(), in, Selection{}); len(got) != 0 || calls.Load() != 0 || reads.Load() != 0 || len(m.Snapshot().Jobs) != 0 {
		t.Fatal("catalog projection triggered work")
	}
	ready := m.Prepare(context.Background(), in, Selection{Probe: true})
	if got := readReady(t, m, in, ready); got != vtt {
		t.Fatalf("body=%q", got)
	}
	if reads.Load() != 0 {
		t.Fatal("working upstream subtitle triggered raw source reads")
	}
	before := calls.Load()
	if len(m.Prepare(context.Background(), in, Selection{})) != 1 || calls.Load() != before {
		t.Fatal("ready projection did work")
	}
}

func TestOwnerItemRevocationAndMoveIsolation(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	var valid atomic.Bool
	valid.Store(true)
	in.Valid = func(context.Context) bool { return valid.Load() }
	ready := m.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, m, in, ready)
	for _, pair := range [][2]string{{"other", in.ItemID}, {in.Owner, "other"}} {
		if _, _, err := m.Open(context.Background(), pair[0], pair[1], ready[0].ID); err == nil {
			t.Fatal("grant crossed owner/item boundary")
		}
	}
	peer := in
	peer.Owner = "peer"
	peer.PlaybackID = "peer-play"
	peerReady := m.Prepare(context.Background(), peer, Selection{})
	if len(peerReady) != 1 || peerReady[0].ID == ready[0].ID {
		t.Fatal("grants not viewer scoped")
	}
	m.MovePlayback(in.Owner, in.PlaybackID, "converted-play")
	m.End(in.Owner, in.PlaybackID)
	_ = readReady(t, m, in, ready)
	m.End(in.Owner, "converted-play")
	if _, _, err := m.Open(context.Background(), in.Owner, in.ItemID, ready[0].ID); err == nil {
		t.Fatal("stopped grant remained valid")
	}
	_ = readReady(t, m, peer, peerReady)
	valid.Store(false)
	if _, _, err := m.Open(context.Background(), peer.Owner, peer.ItemID, peerReady[0].ID); err == nil {
		t.Fatal("revoked viewer read cache")
	}
}

func TestConcurrentPreparationDeduplicatesProbe(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	in.Fetch = func(ctx context.Context, _ Track) ([]byte, string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []byte(vtt), "vtt", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	var wg sync.WaitGroup
	results := make(chan []Ready, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- m.Prepare(context.Background(), in, selected(0)) }()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for r := range results {
		if len(r) != 1 {
			t.Fatalf("ready=%v", r)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("probes=%d", calls.Load())
	}
}

func TestPreviewAndOffNeverReadRawSource(t *testing.T) {
	for _, selection := range []Selection{{Probe: true}, selected(-1)} {
		m := newManager(t, Config{})
		in := input()
		var reads atomic.Int32
		in.Fetch = func(context.Context, Track) ([]byte, string, error) { return nil, "vtt", nil }
		in.Validate = func(context.Context) (string, bool, error) { reads.Add(1); return "v1", true, nil }
		in.Open = func(context.Context, int64, int64) (io.ReadCloser, error) {
			reads.Add(1)
			return nil, errors.New("unexpected")
		}
		if len(m.Prepare(context.Background(), in, selection)) != 0 || reads.Load() != 0 {
			t.Fatal("preview/off performed extraction")
		}
	}
}

func fixtureInput(t *testing.T) (Input, *atomic.Int32) {
	t.Helper()
	data, err := os.ReadFile("../subtitleindex/testdata/indexed-subtitles.mkv")
	if err != nil {
		t.Fatal(err)
	}
	in := input()
	in.Size = int64(len(data))
	in.Tracks = append(in.Tracks, Track{Index: 1, Codec: "subrip", Language: "chi", Name: "Chinese"})
	var reads atomic.Int32
	in.Fetch = func(context.Context, Track) ([]byte, string, error) { return nil, "vtt", nil }
	in.Validate = func(context.Context) (string, bool, error) { return `"version-one"`, true, nil }
	in.Open = func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		reads.Add(1)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if offset < 0 || length < 0 || offset+length > int64(len(data)) {
			return nil, io.ErrUnexpectedEOF
		}
		return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
	}
	return in, &reads
}

func TestIndexedExtractionAfterPreviewAndLanguageReuse(t *testing.T) {
	m := newManager(t, Config{})
	in, reads := fixtureInput(t)
	if len(m.Prepare(context.Background(), in, Selection{Probe: true})) != 0 || reads.Load() != 0 {
		t.Fatal("preview was not metadata only")
	}
	first := m.Prepare(context.Background(), in, selected(0))
	body := readReady(t, m, in, first)
	if !strings.Contains(body, "00:00:01.000 --> 00:00:02.500") {
		t.Fatalf("wrong indexed subtitle: %s", body)
	}
	before := reads.Load()
	second := m.Prepare(context.Background(), in, selected(1))
	if len(second) != 2 {
		t.Fatalf("languages=%v snapshot=%#v", second, m.Snapshot())
	}
	if reads.Load() != before {
		t.Fatalf("switching languages reread shared tiny source: before=%d now=%d", before, reads.Load())
	}
	if m.Snapshot().SourceHitBytes == 0 {
		t.Fatal("no shared source cache hits")
	}
}

func TestStrongArtifactRestartReuseAndVersionRevalidation(t *testing.T) {
	dir := t.TempDir()
	in, reads := fixtureInput(t)
	m, err := New(Config{Dir: dir, PrepareTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	first := m.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, m, in, first)
	if reads.Load() == 0 {
		t.Fatal("fixture not read")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	next := newManager(t, Config{Dir: dir})
	before := reads.Load()
	second := next.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, next, in, second)
	if reads.Load() != before {
		t.Fatal("strong artifact refetched source after restart")
	}
	in.Validate = func(context.Context) (string, bool, error) { return `"version-two"`, true, nil }
	third := next.Prepare(context.Background(), in, Selection{})
	if len(third) != 1 {
		t.Fatal("cached grant missing")
	}
	if _, _, err = next.Open(context.Background(), in.Owner, in.ItemID, third[0].ID); err == nil {
		t.Fatal("changed source served stale artifact")
	}
	if got := next.Prepare(context.Background(), in, Selection{}); len(got) != 0 {
		t.Fatal("known changed source remained advertised")
	}
}

func TestWeakValidatorsPreventIndexedReadsBeforeAndAfterRestart(t *testing.T) {
	dir := t.TempDir()
	in, reads := fixtureInput(t)
	in.Validate = func(context.Context) (string, bool, error) { return "weak-metadata", false, nil }
	for boot := 0; boot < 2; boot++ {
		m, err := New(Config{Dir: dir, PrepareTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if ready := m.Prepare(context.Background(), in, selected(0)); len(ready) != 0 {
			_ = m.Close()
			t.Fatal("weak source advertised an indexed subtitle")
		}
		snapshot := m.Snapshot()
		if snapshot.Jobs[0].Tracks[0].Reason != "source_validator_missing" || snapshot.Jobs[0].Tracks[0].Mode != "indexed" || snapshot.CacheBytes != 0 || snapshot.SourceReadBytes != 0 || reads.Load() != 0 {
			_ = m.Close()
			t.Fatalf("weak source performed indexed work: %#v", snapshot)
		}
		if err = m.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpstreamCaptionsDoNotRequireAStrongMediaValidator(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	var validations, reads atomic.Int32
	in.Validate = func(context.Context) (string, bool, error) { validations.Add(1); return "weak-metadata", false, nil }
	in.Open = func(context.Context, int64, int64) (io.ReadCloser, error) {
		reads.Add(1)
		return nil, errors.New("unexpected media read")
	}
	ready := m.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, m, in, ready)
	if validations.Load() != 0 || reads.Load() != 0 {
		t.Fatal("valid upstream caption accessed source validation or data")
	}
}

func TestReadBudgetFailureDoesNotClaimEmpty(t *testing.T) {
	m := newManager(t, Config{MaxReadBytes: 32})
	in, _ := fixtureInput(t)
	if len(m.Prepare(context.Background(), in, selected(0))) != 0 {
		t.Fatal("limited extraction advertised subtitle")
	}
	s := m.Snapshot()
	if len(s.Jobs) != 1 || s.Jobs[0].Tracks[0].Reason != "resource_limit" {
		t.Fatalf("wrong failure classification: %#v", s)
	}
	if s.Jobs[0].Tracks[0].Mode != "indexed" {
		t.Fatalf("failed indexed attempt lost its mode: %#v", s.Jobs[0].Tracks[0])
	}
}

func TestPinnedArtifactSurvivesFullCache(t *testing.T) {
	m := newManager(t, Config{CacheBytes: int64(len(vtt))})
	in := input()
	ready := m.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, m, in, ready)
	other := input()
	other.Owner = "other"
	other.ItemID = "other-item"
	other.SourceKey = "other-source"
	other.SourceID = "other-source"
	if got := m.Prepare(context.Background(), other, selected(0)); len(got) != 0 {
		t.Fatal("capacity overflow advertised new track")
	}
	_ = readReady(t, m, in, ready)
	if m.Snapshot().CacheBytes > int64(len(vtt)) {
		t.Fatal("cache exceeded hard budget")
	}
}

func TestWorkerCapacityAndLastViewerCancellation(t *testing.T) {
	m := newManager(t, Config{Workers: 1, PrepareTimeout: 10 * time.Millisecond})
	in := input()
	started := make(chan struct{})
	canceled := make(chan struct{})
	in.Fetch = func(ctx context.Context, _ Track) ([]byte, string, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, "", ctx.Err()
	}
	_ = m.Prepare(context.Background(), in, selected(0))
	<-started
	peer := in
	peer.Owner = "peer"
	peer.PlaybackID = "peer-play"
	_ = m.Prepare(context.Background(), peer, Selection{})
	m.End(in.Owner, in.PlaybackID)
	select {
	case <-canceled:
		t.Fatal("one viewer canceled peer work")
	default:
	}
	other := input()
	other.SourceKey = "other-source"
	var calls atomic.Int32
	other.Fetch = func(context.Context, Track) ([]byte, string, error) { calls.Add(1); return []byte(vtt), "vtt", nil }
	if got := m.Prepare(context.Background(), other, selected(0)); len(got) != 0 || calls.Load() != 0 {
		t.Fatal("worker capacity was exceeded")
	}
	m.End(peer.Owner, peer.PlaybackID)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("last viewer did not cancel work")
	}
}

func TestContextCancellationAndIdempotentClose(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	started := make(chan struct{})
	in.Fetch = func(ctx context.Context, _ Track) ([]byte, string, error) {
		close(started)
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Prepare(ctx, in, selected(0)); close(done) }()
	<-started
	cancel()
	<-done
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Prepare(context.Background(), in, selected(0)); len(got) != 0 {
		t.Fatal("closed manager admitted work")
	}
}

func TestStoreOwnershipLockAndPersistenceBounds(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if second, err := New(Config{Dir: dir}); err == nil {
		_ = second.Close()
		t.Fatal("two managers shared storage")
	}
	_ = m.Close()
	other := t.TempDir()
	cache := filepath.Join(other, "eag-subtitle-artifacts-v1")
	if err = os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cache, "unrelated")
	if err = os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if bad, err := New(Config{Dir: other}); err == nil {
		_ = bad.Close()
		t.Fatal("foreign directory accepted")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatal("foreign file changed")
	}
}

func TestTextValidation(t *testing.T) {
	for _, tc := range []struct {
		name, format, data string
		valid              bool
	}{
		{"vtt", "vtt", vtt, true}, {"srt", "srt", "1\n00:00:01,000 --> 00:00:03,000\nText\n", true},
		{"ass", "ass", "[Script Info]\nTitle: Test\n[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Text\n", true},
		{"empty", "vtt", "", false}, {"header-only", "vtt", "WEBVTT\n", false}, {"html", "vtt", "<!doctype html><html>failure</html>", false}, {"binary", "subrip", "\x00abc", false}, {"image", "pgssub", "nonempty", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, format, cues, err := normalizeText([]byte(tc.data), tc.format)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.valid && (len(data) == 0 || cues == 0 || format != "vtt" && format != "ass") {
				t.Fatal("invalid normalized result")
			}
		})
	}
}

func TestRecoverPreviewRequiresExplicitAdmissionAndRespectsOff(t *testing.T) {
	for _, index := range []int{0, -1} {
		m := newManager(t, Config{})
		in, reads := fixtureInput(t)
		ready := m.Prepare(context.Background(), in, Selection{Probe: true, Recover: true, Index: &index})
		if index == 0 {
			if len(ready) != 1 || reads.Load() == 0 {
				t.Fatalf("explicit recovery not prepared: %#v", m.Snapshot())
			}
		} else if len(ready) != 0 || reads.Load() != 0 {
			t.Fatal("explicitOff admitted recovery")
		}
	}
}

func TestProbeBatchPrioritizesSelectedAndExternalTracks(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	in.Tracks = nil
	for i := 0; i < 32; i++ {
		in.Tracks = append(in.Tracks, Track{Index: i, Codec: "subrip"})
	}
	in.Tracks[31].External = true
	in.Tracks[31].Codec = "ass"
	var mu sync.Mutex
	var calls []int
	in.Fetch = func(_ context.Context, t Track) ([]byte, string, error) {
		mu.Lock()
		calls = append(calls, t.Index)
		mu.Unlock()
		return nil, "vtt", nil
	}
	_ = m.Prepare(context.Background(), in, selected(10))
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 || calls[0] != 10 || calls[1] != 31 {
		t.Fatalf("probe order/budget: %v", calls)
	}
}

func TestCaptionlessVTTCanRecoverButHTMLCannot(t *testing.T) {
	for _, body := range []string{"WEBVTT\n", "<!doctype html><html>upstream error</html>"} {
		m := newManager(t, Config{})
		in, reads := fixtureInput(t)
		in.Fetch = func(context.Context, Track) ([]byte, string, error) { return []byte(body), "vtt", nil }
		ready := m.Prepare(context.Background(), in, selected(0))
		if strings.HasPrefix(body, "WEBVTT") {
			if len(ready) != 1 || reads.Load() == 0 {
				t.Fatal("empty VTT did not recover")
			}
		} else if len(ready) != 0 || reads.Load() != 0 {
			t.Fatal("HTML error triggered recovery")
		}
	}
}

func TestSnapshotCopiesAndSanitizesRuntimeData(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	in.SourceName = "https://upstream/secret?api_key=value"
	in.Tracks[0].Name = "English\ncontrol"
	_ = m.Prepare(context.Background(), in, selected(0))
	first := m.Snapshot()
	if first.Jobs[0].SourceName != "[media source]" || first.Jobs[0].Tracks[0].Name != "Englishcontrol" {
		t.Fatal("snapshot contained unsafe labels")
	}
	first.Jobs[0].Tracks[0].State = "modified"
	if m.Snapshot().Jobs[0].Tracks[0].State == "modified" {
		t.Fatal("snapshot aliases state")
	}
}

func waitIdle(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for m.HasActiveWork() {
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSourcePreparationDeadlinePreservesEarlierGrants(t *testing.T) {
	m := newManager(t, Config{})
	first := input()
	ready := m.Prepare(context.Background(), first, selected(0))
	_ = readReady(t, m, first, ready)
	waitIdle(t, m)
	second := first
	second.SourceID, second.SourceKey = "second-source", "second-source-key"
	started := make(chan struct{})
	release := make(chan struct{})
	second.Fetch = func(ctx context.Context, _ Track) ([]byte, string, error) {
		close(started)
		select {
		case <-release:
			return []byte(vtt), "vtt", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if got := m.Prepare(ctx, second, selected(0)); len(got) != 0 {
		t.Fatal("blocked source became ready")
	}
	<-started
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("shared preparation budget did not expire")
	}
	_ = readReady(t, m, first, ready)
	close(release)
	waitIdle(t, m)
	if got := m.Prepare(context.Background(), second, Selection{}); len(got) != 1 {
		t.Fatal("bounded preparation lease was canceled by budget deadline")
	}
	_ = readReady(t, m, first, ready)
}

func TestCanceledPreparationOnlyDetachesItsNewSourceAdmission(t *testing.T) {
	m := newManager(t, Config{})
	first := input()
	ready := m.Prepare(context.Background(), first, selected(0))
	waitIdle(t, m)
	second := first
	second.SourceID, second.SourceKey = "second-source", "second-source-key"
	started, stopped := make(chan struct{}), make(chan struct{})
	second.Fetch = func(ctx context.Context, _ Track) ([]byte, string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Prepare(ctx, second, selected(0)) }()
	<-started
	cancel()
	<-done
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("new canceled admission kept worker alive")
	}
	_ = readReady(t, m, first, ready)
}

func TestCanceledPreviewDoesNotRevokeAnActiveAdmission(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	ready := m.Prepare(context.Background(), in, selected(0))
	waitIdle(t, m)
	in.Tracks = append(in.Tracks, Track{Index: 1, Codec: "subrip"})
	started, release := make(chan struct{}), make(chan struct{})
	in.Fetch = func(ctx context.Context, track Track) ([]byte, string, error) {
		if track.Index == 0 {
			return []byte(vtt), "vtt", nil
		}
		close(started)
		select {
		case <-release:
			return []byte(vtt), "vtt", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Prepare(ctx, in, Selection{Probe: true}) }()
	<-started
	cancel()
	<-done
	_ = readReady(t, m, in, ready)
	close(release)
	waitIdle(t, m)
	if got := m.Prepare(context.Background(), in, Selection{}); len(got) != 2 {
		t.Fatal("prior active admission lost its bounded preparation")
	}
}

func TestMovePlaybackMergesDestinationAndSurvivesLateOldStop(t *testing.T) {
	m := newManager(t, Config{})
	old := input()
	old.Tracks = append(old.Tracks, Track{Index: 1, Codec: "subrip", Language: "chi"})
	oldReady := m.Prepare(context.Background(), old, selected(0))
	waitIdle(t, m)
	if len(oldReady) == 0 {
		t.Fatal("old playback has no ready captions")
	}
	current := old
	current.PlaybackID = "new-playback"
	currentReady := m.Prepare(context.Background(), current, selected(1))
	if len(currentReady) != 2 {
		t.Fatalf("new playback captions = %#v", currentReady)
	}
	peer := old
	peer.Owner, peer.PlaybackID = "peer", "peer-playback"
	peerReady := m.Prepare(context.Background(), peer, selected(0))

	m.MovePlayback(old.Owner, old.PlaybackID, current.PlaybackID)
	m.MovePlayback(old.Owner, old.PlaybackID, current.PlaybackID) // duplicate negotiation is harmless
	m.mu.Lock()
	j := m.jobs[hash(old.SourceKey, old.SourceID)]
	v := j.viewers[viewerKey(old.Owner, current.PlaybackID)]
	if len(j.viewers) != 2 || v == nil || v.selected == nil || *v.selected != 1 || v.input.PlaybackID != current.PlaybackID {
		m.mu.Unlock()
		t.Fatal("replacement lost destination selection or retained an old lease")
	}
	m.mu.Unlock()

	m.End(old.Owner, old.PlaybackID) // Emby may stop the replaced encoding late.
	for _, r := range append(oldReady, currentReady...) {
		_ = readReady(t, m, current, []Ready{r})
	}
	for _, r := range peerReady {
		_ = readReady(t, m, peer, []Ready{r})
	}
	m.End(current.Owner, current.PlaybackID)
	for _, r := range append(oldReady, currentReady...) {
		if _, _, err := m.Open(context.Background(), current.Owner, current.ItemID, r.ID); err == nil {
			t.Fatal("new playback stop left a replacement grant active")
		}
	}
	for _, r := range peerReady {
		_ = readReady(t, m, peer, []Ready{r})
	}
}

func blockedPreview(t *testing.T, m *Manager) (Input, <-chan struct{}) {
	t.Helper()
	in, _ := fixtureInput(t)
	in.PlaybackID = "preview:item:source"
	started, stopped := make(chan struct{}), make(chan struct{})
	in.Open = func(ctx context.Context, _, _ int64) (io.ReadCloser, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	index := 0
	_ = m.Prepare(context.Background(), in, Selection{Probe: true, Recover: true, Index: &index})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("preview did not start indexed work")
	}
	return in, stopped
}

func TestActualPlaybackAdoptsPreviewAndLastStopCancelsWork(t *testing.T) {
	m := newManager(t, Config{PrepareTimeout: 10 * time.Millisecond})
	preview, stopped := blockedPreview(t, m)
	actual := preview
	actual.PlaybackID = "actual-playback"
	index := 0
	_ = m.Prepare(context.Background(), actual, Selection{Playback: true, Index: &index})
	if s := m.Snapshot(); len(s.Jobs) != 1 || s.Jobs[0].Viewers != 1 {
		t.Fatalf("preview lease remained independent: %#v", s)
	}
	m.End(preview.Owner, preview.PlaybackID)
	select {
	case <-stopped:
		t.Fatal("late preview stop canceled actual playback work")
	default:
	}
	m.End(actual.Owner, actual.PlaybackID)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("last playback stop left preview extraction alive")
	}
	waitIdle(t, m)
}

func TestActualOffRetiresPreviewWithoutCancelingPeerRecovery(t *testing.T) {
	for _, withPeer := range []bool{false, true} {
		m := newManager(t, Config{PrepareTimeout: 10 * time.Millisecond})
		preview, stopped := blockedPreview(t, m)
		index := 0
		peer := preview
		peer.Owner, peer.PlaybackID = "peer", "peer-playback"
		if withPeer {
			_ = m.Prepare(context.Background(), peer, Selection{Playback: true, Index: &index})
		}
		actual := preview
		actual.PlaybackID = "actual-playback"
		off := -1
		_ = m.Prepare(context.Background(), actual, Selection{Playback: true, Index: &off})
		if withPeer {
			select {
			case <-stopped:
				t.Fatal("Off canceled another viewer's recovery")
			default:
			}
			m.End(actual.Owner, actual.PlaybackID)
			select {
			case <-stopped:
				t.Fatal("stopping Off viewer canceled peer")
			default:
			}
			m.End(peer.Owner, peer.PlaybackID)
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("obsolete preview work was not canceled")
		}
		waitIdle(t, m)
	}
}

func TestAdoptedPreviewGrantsFollowActualWithoutAffectingOtherSources(t *testing.T) {
	m := newManager(t, Config{})
	preview := input()
	preview.PlaybackID = "preview:item:source"
	index := 0
	previewReady := m.Prepare(context.Background(), preview, Selection{Probe: true, Recover: true, Index: &index})
	waitIdle(t, m)
	other := preview
	other.SourceID, other.SourceKey, other.PlaybackID = "other-source", "other-key", "other-preview"
	otherReady := m.Prepare(context.Background(), other, Selection{Probe: true})
	waitIdle(t, m)
	peer := preview
	peer.Owner, peer.PlaybackID = "peer", "peer-preview"
	peerReady := m.Prepare(context.Background(), peer, Selection{})
	actual := preview
	actual.PlaybackID = "actual-playback"
	actualReady := m.Prepare(context.Background(), actual, selected(0))
	m.End(preview.Owner, preview.PlaybackID)
	_ = readReady(t, m, actual, previewReady)
	_ = readReady(t, m, actual, actualReady)
	m.End(actual.Owner, actual.PlaybackID)
	if _, _, err := m.Open(context.Background(), actual.Owner, actual.ItemID, previewReady[0].ID); err == nil {
		t.Fatal("adopted preview grant outlived actual playback")
	}
	_ = readReady(t, m, other, otherReady)
	_ = readReady(t, m, peer, peerReady)
}

func TestActualOffCancelsPreviewWhenUpstreamReusesPlaybackID(t *testing.T) {
	m := newManager(t, Config{PrepareTimeout: 10 * time.Millisecond})
	preview, stopped := blockedPreview(t, m)
	off := -1
	_ = m.Prepare(context.Background(), preview, Selection{Playback: true, Index: &off})
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("reused playback ID retained obsolete preview work")
	}
	waitIdle(t, m)
}

func TestReadyTrackAuthorizationRunsOutsideLockAndAppliesToCachedGrants(t *testing.T) {
	m := newManager(t, Config{})
	in := input()
	var allowed atomic.Bool
	allowed.Store(true)
	in.ValidTrack = func(_ context.Context, track Track) bool {
		_ = m.Snapshot() // Deadlocks if a callback runs while Manager.mu is held.
		return allowed.Load() && track.Index == 0
	}
	ready := m.Prepare(context.Background(), in, selected(0))
	_ = readReady(t, m, in, ready)
	allowed.Store(false)
	if got := m.Prepare(context.Background(), in, Selection{}); len(got) != 0 {
		t.Fatal("revoked track remained in ready projection")
	}
	if _, _, err := m.Open(context.Background(), in.Owner, in.ItemID, ready[0].ID); err == nil {
		t.Fatal("revoked track grant remained readable")
	}
}
