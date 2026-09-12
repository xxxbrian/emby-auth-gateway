package subtitleindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureOptions struct {
	count           int
	text            string
	noCues          bool
	missingDuration bool
	missingRelative bool
	wrongTrack      bool
	badTimestamp    bool
	laced           bool
	compressed      bool
	duplicateCue    bool
	badCluster      bool
}
type sparseFixture struct {
	size            int64
	pieces          map[int64][]byte
	bytes, requests atomic.Int64
}

func (f *sparseFixture) source() Source {
	return Source{Size: f.size, Container: "mkv", Open: func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if offset < 0 || size < 0 || offset > f.size || size > f.size-offset {
			return nil, fmt.Errorf("unexpected range")
		}
		f.requests.Add(1)
		f.bytes.Add(size)
		b := make([]byte, int(size))
		for start, piece := range f.pieces {
			end := start + int64(len(piece))
			a, z := max(offset, start), min(offset+size, end)
			if a < z {
				copy(b[a-offset:z-offset], piece[a-start:z-start])
			}
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}}
}

func ebmlSize(size uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, size)
	b[0] |= 1
	return b
}
func ebmlID(id uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, id)
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}
func node(id uint64, parts ...[]byte) []byte {
	b := bytes.Join(parts, nil)
	return bytes.Join([][]byte{ebmlID(id), ebmlSize(uint64(len(b))), b}, nil)
}
func uintNode(id, n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return node(id, b)
}
func trackNode(number, kind uint64, codec, language string, compressed bool) []byte {
	parts := [][]byte{uintNode(0xd7, number), uintNode(0x83, kind), node(0x86, []byte(codec)), node(0x22b59c, []byte(language))}
	if compressed {
		parts = append(parts, node(0x6d80, node(0x6240)))
	}
	return node(0xae, parts...)
}
func newFixture(o fixtureOptions) *sparseFixture {
	if o.count == 0 {
		o.count = 2
	}
	if o.text == "" {
		o.text = "你好 <世界> & friends\nsecond line"
	}
	f := &sparseFixture{size: 64 << 30, pieces: make(map[int64][]byte)}
	ebml := node(0x1a45dfa3, node(0x4282, []byte("matroska")))
	f.pieces[0] = ebml
	segmentHeader := bytes.Join([][]byte{ebmlID(idSegment), ebmlSize(uint64(f.size - int64(len(ebml)) - 12))}, nil)
	f.pieces[int64(len(ebml))] = segmentHeader
	base := int64(len(ebml) + len(segmentHeader))
	infoOffset, tracksOffset, cuesOffset := base+4096, base+8192, int64(63<<30)
	seek := func(id uint64, offset int64) []byte {
		return node(0x4dbb, node(0x53ab, ebmlID(id)), uintNode(0x53ac, uint64(offset-base)))
	}
	seekHead := node(idSeekHead, seek(idInfo, infoOffset), seek(idTracks, tracksOffset), seek(idCues, cuesOffset))
	f.pieces[base] = seekHead
	void := func(start, end int64) {
		f.pieces[start] = bytes.Join([][]byte{ebmlID(0xec), ebmlSize(uint64(end - start - 9))}, nil)
	}
	void(base+int64(len(seekHead)), infoOffset)
	info := node(idInfo, uintNode(0x2ad7b1, 1_000_000))
	f.pieces[infoOffset] = info
	void(infoOffset+int64(len(info)), tracksOffset)
	// Nonconsecutive TrackNumbers establish that stream index is not track ID.
	tracks := node(idTracks, trackNode(3, 1, "V_MPEG4/ISO/AVC", "und", false), trackNode(8, 2, "A_AAC", "und", false), trackNode(42, 17, "S_TEXT/UTF8", "chi", o.compressed), trackNode(77, 17, "S_TEXT/UTF8", "eng", false))
	f.pieces[tracksOffset] = tracks
	firstCluster := int64(50 << 30)
	void(tracksOffset+int64(len(tracks)), firstCluster)
	var cues [][]byte
	for i := 0; i < o.count; i++ {
		clusterOffset := firstCluster + int64(i)*8192
		timestamp := uint64(i*2000 + 1000)
		duration := uint64(1500)
		timeNode := uintNode(0xe7, timestamp)
		var groups [][]byte
		for language, n := range []uint64{42, 77} {
			actual := n
			if o.wrongTrack && language == 0 {
				actual = 41
			}
			flag := byte(0)
			if o.laced && language == 0 {
				flag = 2
			}
			relative := []byte{0, 0}
			if o.badTimestamp && language == 0 {
				relative = []byte{0, 1}
			}
			text := o.text
			if language == 1 {
				text = "English text"
			}
			payload := append([]byte{byte(0x80 | actual), relative[0], relative[1], flag}, []byte(text)...)
			group := node(0xa0, node(0xa1, payload), uintNode(0x9b, duration))
			groups = append(groups, group)
			relOffset := len(timeNode)
			if language == 1 {
				relOffset += len(groups[0])
			}
			positions := [][]byte{uintNode(0xf7, n), uintNode(0xf1, uint64(clusterOffset-base))}
			if !o.missingRelative || language == 1 {
				positions = append(positions, uintNode(0xf0, uint64(relOffset)))
			}
			if !o.missingDuration || language == 1 {
				positions = append(positions, uintNode(0xb2, duration))
			}
			point := node(0xbb, uintNode(0xb3, timestamp), node(0xb7, positions...))
			if !o.noCues || language == 1 {
				cues = append(cues, point)
				if o.duplicateCue && language == 0 && i == 0 {
					cues = append(cues, point)
				}
			}
		}
		clusterID := uint64(idCluster)
		if o.badCluster {
			clusterID = idInfo
		}
		f.pieces[clusterOffset] = node(clusterID, timeNode, groups[0], groups[1])
	}
	f.pieces[cuesOffset] = node(idCues, cues...)
	return f
}

func TestExtractSparse64GiBMultilingualSource(t *testing.T) {
	f := newFixture(fixtureOptions{count: 688})
	// The source adapter caches byte ranges, just as the shared source cache
	// does. Subsequent languages must not imply a scan or depend on the first.
	source := f.source()
	original := source.Open
	cache := make(map[[2]int64][]byte)
	var mu sync.Mutex
	source.Open = func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		key := [2]int64{offset, size}
		mu.Lock()
		b, ok := cache[key]
		mu.Unlock()
		if !ok {
			body, err := original(ctx, offset, size)
			if err != nil {
				return nil, err
			}
			b, err = io.ReadAll(body)
			body.Close()
			if err != nil {
				return nil, err
			}
			mu.Lock()
			cache[key] = b
			mu.Unlock()
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	zh, err := Extract(context.Background(), source, Track{Index: 2, Codec: "subrip", Language: "zho"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if zh.Cues != 688 || zh.Format != "vtt" || !strings.Contains(string(zh.Data), "你好 &lt;世界&gt; &amp; friends") {
		t.Fatalf("unexpected subtitle: cues=%d format=%s", zh.Cues, zh.Format)
	}
	if !bytes.HasPrefix(zh.Data, []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.500\n")) {
		t.Fatalf("wrong timing: %.100s", zh.Data)
	}
	if zh.ReadBytes > 8<<20 || zh.Requests > 40 {
		t.Fatalf("unbounded sparse read: bytes=%d requests=%d", zh.ReadBytes, zh.Requests)
	}
	before := f.bytes.Load()
	en, err := Extract(context.Background(), source, Track{Index: 3, Codec: "srt", Language: "en"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if en.Cues != 688 || !bytes.Contains(en.Data, []byte("English text")) || bytes.Contains(en.Data, []byte("你好")) {
		t.Fatal("languages were confused")
	}
	if more := f.bytes.Load() - before; more > 64<<10 {
		t.Fatalf("language switch did not reuse sparse reads: %d", more)
	}
	t.Logf("64GiB source,688 cues/language: first read=%d bytes/%d requests; second additional origin bytes=%d", zh.ReadBytes, zh.Requests, f.bytes.Load()-before)
}

func TestMalformedOrMissingIndexNeverLooksEmpty(t *testing.T) {
	tests := []struct {
		name    string
		options fixtureOptions
		track   Track
		want    error
	}{
		{"missing selected cues", fixtureOptions{noCues: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"missing duration", fixtureOptions{missingDuration: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"missing relative offset", fixtureOptions{missingRelative: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"wrong block track", fixtureOptions{wrongTrack: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"timestamp mismatch", fixtureOptions{badTimestamp: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"laced text", fixtureOptions{laced: true}, Track{Index: 2, Codec: "subrip"}, ErrUnsupported},
		{"compressed text", fixtureOptions{compressed: true}, Track{Index: 2, Codec: "subrip"}, ErrUnsupported},
		{"duplicate cue", fixtureOptions{duplicateCue: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"bad cluster", fixtureOptions{badCluster: true}, Track{Index: 2, Codec: "subrip"}, ErrIndex},
		{"language mismatch", fixtureOptions{}, Track{Index: 2, Codec: "subrip", Language: "eng"}, ErrIndex},
		{"track number used as index", fixtureOptions{}, Track{Index: 42, Codec: "subrip"}, ErrIndex},
		{"video stream", fixtureOptions{}, Track{Index: 0, Codec: "subrip"}, ErrUnsupported},
		{"ASS unsupported", fixtureOptions{}, Track{Index: 2, Codec: "ass"}, ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Extract(context.Background(), newFixture(tt.options).source(), tt.track, Limits{})
			if !errors.Is(err, tt.want) || errors.Is(err, ErrEmpty) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			if len(result.Data) > 0 {
				t.Fatal("partial subtitle published after failure")
			}
		})
	}
}

func TestResourceLimitsAndEmptyPackets(t *testing.T) {
	for name, limits := range map[string]Limits{"bytes": {MaxReadBytes: 1024}, "requests": {MaxRequests: 1}, "cues": {MaxCues: 1}, "output": {MaxOutputBytes: 12}, "negative": {MaxRequests: -1}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(fixtureOptions{})
			result, err := Extract(context.Background(), f.source(), Track{Index: 2, Codec: "subrip"}, limits)
			if !errors.Is(err, ErrLimit) {
				t.Fatalf("want limit, got %v", err)
			}
			if result.Data != nil {
				t.Fatal("partial output")
			}
			if limits.MaxReadBytes > 0 && result.ReadBytes > limits.MaxReadBytes {
				t.Fatal("read byte budget exceeded")
			}
			if limits.MaxRequests > 0 && result.Requests > limits.MaxRequests {
				t.Fatal("request budget exceeded")
			}
		})
	}
	f := newFixture(fixtureOptions{text: " \r\n "})
	result, err := Extract(context.Background(), f.source(), Track{Index: 2, Codec: "subrip"}, Limits{})
	if !errors.Is(err, ErrEmpty) || result.Data != nil {
		t.Fatalf("expected validated empty, got %v", err)
	}
}

func TestCancellationAndSourceFailure(t *testing.T) {
	f := newFixture(fixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Extract(ctx, f.source(), Track{Index: 2, Codec: "subrip"}, Limits{})
	if !errors.Is(err, context.Canceled) || f.requests.Load() != 0 {
		t.Fatalf("cancelled read made requests: %v", err)
	}
	for name, open := range map[string]func(context.Context, int64, int64) (io.ReadCloser, error){
		"short body": func(context.Context, int64, int64) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte{1, 2})), nil
		},
		"nil body":     func(context.Context, int64, int64) (io.ReadCloser, error) { return nil, nil },
		"origin error": func(context.Context, int64, int64) (io.ReadCloser, error) { return nil, io.ErrClosedPipe },
	} {
		t.Run(name, func(t *testing.T) {
			source := f.source()
			source.Open = open
			result, err := Extract(context.Background(), source, Track{Index: 2, Codec: "subrip"}, Limits{})
			if !errors.Is(err, ErrSource) || result.Data != nil {
				t.Fatalf("got %v", err)
			}
		})
	}
	source := f.source()
	source.Open = func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = Extract(ctx, source, Track{Index: 2, Codec: "subrip"}, Limits{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost timeout: %v", err)
	}
}

func TestBoundsAndTextSafety(t *testing.T) {
	f := newFixture(fixtureOptions{text: "one\n\n00:00:00.000 --> 99:00:00.000\n<script>x</script>"})
	result, err := Extract(context.Background(), f.source(), Track{Index: 2, Codec: "subrip"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.Data, []byte("one\n\u200b\n00:00:00.000 --&gt;")) || bytes.Contains(result.Data, []byte("<script>")) {
		t.Fatal("cue payload escaped into WebVTT structure")
	}
	r := &reader{ctx: context.Background(), source: f.source(), limits: Limits{MaxReadBytes: 4096, MaxRequests: 4}, pages: make(map[int64][]byte)}
	for _, w := range []interval{{-1, 2}, {0, f.size + 1}, {f.size, 1}, {1 << 62, 1<<62 + 1}} {
		if err := r.prefetch([]interval{w}); !errors.Is(err, ErrIndex) {
			t.Fatalf("accepted out of range %+v: %v", w, err)
		}
	}
	for _, data := range [][]byte{{}, {0}, {0x1f}, {0xa0, 0xff}, {0xa0, 0x88, 1}} {
		if err := children(data, func(uint64, []byte) error { return nil }); len(data) > 0 && !errors.Is(err, ErrIndex) {
			t.Fatalf("malformed EBML accepted: %x", data)
		}
	}
}

func TestFFmpegMatroskaRoundTrip(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("GATEWAY_REQUIRE_FFMPEG_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("FFmpeg unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "source.srt")
	if err := os.WriteFile(input, []byte("1\n00:00:01,000 --> 00:00:02,500\n你好\n\n2\n00:00:04,000 --> 00:00:05,000\nSecond subtitle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "source.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "srt", "-i", input, "-map", "0:0", "-c:s", "srt", media)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, b)
	}
	f, err := os.Open(media)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Size: stat.Size(), Container: "mkv", Open: func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return io.NopCloser(io.NewSectionReader(f, offset, size)), nil
	}}
	result, err := Extract(ctx, source, Track{Index: 0, Codec: "subrip"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cues != 2 || !bytes.Contains(result.Data, []byte("00:00:04.000 --> 00:00:05.000\nSecond subtitle")) || !bytes.Contains(result.Data, []byte("你好")) {
		t.Fatalf("incorrect round trip: %s", result.Data)
	}
}

func TestCommittedTwoLanguageFixture(t *testing.T) {
	f, err := os.Open("testdata/indexed-subtitles.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Size: stat.Size(), Container: "mkv", Open: func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(f, offset, size)), nil
	}}
	for _, want := range []struct {
		index          int
		language, text string
	}{{0, "eng", "English later cue"}, {1, "chi", "后面的中文字幕"}} {
		result, err := Extract(context.Background(), source, Track{Index: want.index, Codec: "subrip", Language: want.language}, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Cues != 2 || !bytes.Contains(result.Data, []byte("00:00:20.000 --> 00:00:21.000\n"+want.text)) {
			t.Fatalf("unexpected subtitle index%d: %s", want.index, result.Data)
		}
	}
}

func TestPrefetchConcurrencyAndCancellation(t *testing.T) {
	var active, peak, closed atomic.Int64
	source := Source{Size: 1 << 30, Container: "mkv", Open: func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-ctx.Done():
			active.Add(-1)
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
		return &testCloseReader{Reader: bytes.NewReader(make([]byte, int(size))), close: func() { active.Add(-1); closed.Add(1) }}, nil
	}}
	limits, _ := Limits{}.normalized()
	r := &reader{ctx: context.Background(), source: source, limits: limits, pages: make(map[int64][]byte)}
	var windows []interval
	for i := int64(0); i < 32; i++ {
		windows = append(windows, interval{i << 20, (i << 20) + 32})
	}
	if err := r.prefetch(windows); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 4 || active.Load() != 0 || closed.Load() != 32 || r.requests != 32 {
		t.Fatalf("wrong bounded concurrency or ownership: peak=%d active=%d closed=%d requests=%d", peak.Load(), active.Load(), closed.Load(), r.requests)
	}
}

type testCloseReader struct {
	io.Reader
	close func()
}

func (r *testCloseReader) Close() error { r.close(); return nil }

func FuzzMalformedEBML(f *testing.F) {
	for _, b := range [][]byte{{0}, node(idSegment, node(idInfo)), node(0x1a45dfa3, node(0x4282, []byte("matroska"))), {0xa0, 0xff}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			t.Skip()
		}
		source := Source{Size: int64(len(b)), Container: "mkv", Open: func(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
			if offset < 0 || size < 0 || offset > int64(len(b)) || size > int64(len(b))-offset {
				t.Fatalf("invalid source read offset=%d size=%d", offset, size)
			}
			return io.NopCloser(bytes.NewReader(b[offset : offset+size])), nil
		}}
		_, _ = Extract(context.Background(), source, Track{Index: 0, Codec: "subrip"}, Limits{MaxReadBytes: 1 << 20, MaxRequests: 16, MaxOutputBytes: 4096, MaxCues: 10})
	})
}
