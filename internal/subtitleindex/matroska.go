package subtitleindex

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	idSegment              = 0x18538067
	idSeekHead             = 0x114d9b74
	idInfo                 = 0x1549a966
	idTracks               = 0x1654ae6b
	idCues                 = 0x1c53bb6b
	idCluster              = 0x1f43b675
	maxMetadataBytes int64 = 24 << 20
	maxPacketBytes   int64 = 256 << 10
)

// Extract returns all validated subtitle packets named by the track's Cue index.
// Cues do not guarantee coverage of uncued packets; missing track cues are an
// index failure, never evidence that the underlying subtitle stream is empty.
// No fallback scans media clusters. A deadline is enforced even if the caller
// does not provide one; callers can supply a shorter deadline.
func Extract(ctx context.Context, source Source, track Track, limits Limits) (result Result, err error) {
	if source.Size <= 0 || source.Open == nil {
		return result, ErrSource
	}
	if strings.ToLower(source.Container) != "mkv" && strings.ToLower(source.Container) != "matroska" {
		return result, ErrUnsupported
	}
	if track.Index < 0 {
		return result, ErrIndex
	}
	if normalizeCodec(track.Codec) != "subrip" {
		return result, ErrUnsupported
	}
	limits, err = limits.normalized()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r := &reader{ctx: ctx, source: source, limits: limits, pages: make(map[int64][]byte)}
	defer func() { result.ReadBytes = r.used; result.Requests = r.requests }()
	result.Data, result.Cues, err = extractMatroska(r, track)
	if err == nil {
		result.Format = "vtt"
	} else {
		result.Data = nil
		result.Cues = 0
	}
	return result, err
}

type cue struct{ cluster, relative, time, duration uint64 }
type selectedTrack struct{ number uint64 }
type packet struct {
	time, duration uint64
	text           string
}

func extractMatroska(r *reader, track Track) ([]byte, int, error) {
	segment, positions, err := locate(r)
	if err != nil {
		return nil, 0, err
	}
	var loaded int64
	load := func(id uint64) ([]byte, error) {
		pos, ok := positions[id]
		if !ok {
			return nil, ErrIndex
		}
		e, err := r.element(pos, segment.end)
		if err != nil {
			return nil, err
		}
		if e.id != id || e.unknown {
			return nil, ErrIndex
		}
		if e.end-e.start > maxMetadataBytes-loaded {
			return nil, ErrLimit
		}
		loaded += e.end - e.start
		return r.read(e.start, e.end-e.start)
	}
	info, err := load(idInfo)
	if err != nil {
		return nil, 0, err
	}
	scale := uint64(1_000_000)
	if err := children(info, func(id uint64, b []byte) error {
		if id == 0x2ad7b1 {
			var e error
			scale, e = unsigned(b)
			return e
		}
		// Linked segments require a timeline outside this single source.
		if id == 0x3cb923 || id == 0x3eb923 {
			return ErrUnsupported
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}
	if _, err := milliseconds(1, scale); err != nil {
		return nil, 0, err
	}
	tracks, err := load(idTracks)
	if err != nil {
		return nil, 0, err
	}
	selected, err := selectTrack(tracks, track)
	if err != nil {
		return nil, 0, err
	}
	index, err := load(idCues)
	if err != nil {
		return nil, 0, err
	}
	cues, err := readCues(index, selected.number, r.limits.MaxCues, segment.end-segment.start, scale)
	if err != nil {
		return nil, 0, err
	}
	// Fetch cluster headers together, then block headers together. Requests are
	// batched before dispatch so known over-budget plans do not partially run.
	windows := make([]interval, 0, len(cues))
	for _, c := range cues {
		p := segment.start + int64(c.cluster)
		windows = append(windows, interval{p, p + min(64, segment.end-p)})
	}
	if err := r.prefetch(windows); err != nil {
		return nil, 0, err
	}
	type location struct {
		cluster   element
		timestamp uint64
		offset    int64
	}
	locations := make([]location, len(cues))
	clusterTimes := make(map[int64]uint64)
	windows = windows[:0]
	for i, c := range cues {
		pos := segment.start + int64(c.cluster)
		cl, err := r.element(pos, segment.end)
		if err != nil {
			return nil, 0, err
		}
		if cl.id != idCluster || cl.unknown {
			return nil, 0, ErrIndex
		}
		if c.relative >= uint64(cl.end-cl.start) {
			return nil, 0, ErrIndex
		}
		timestamp, ok := clusterTimes[pos]
		if !ok {
			timestamp, err = clusterTime(r, cl)
			if err != nil {
				return nil, 0, err
			}
			clusterTimes[pos] = timestamp
		}
		offset := cl.start + int64(c.relative)
		locations[i] = location{cl, timestamp, offset}
		windows = append(windows, interval{offset, offset + min(512, cl.end-offset)})
	}
	if err := r.prefetch(windows); err != nil {
		return nil, 0, err
	}
	blocks := make([]element, len(cues))
	windows = windows[:0]
	for i, loc := range locations {
		e, err := r.element(loc.offset, loc.cluster.end)
		if err != nil {
			return nil, 0, err
		}
		if e.id != 0xa0 || e.unknown {
			return nil, 0, ErrIndex
		}
		if e.end-e.start > maxPacketBytes {
			return nil, 0, ErrLimit
		}
		blocks[i] = e
		windows = append(windows, interval{e.start, e.end})
	}
	if err := r.prefetch(windows); err != nil {
		return nil, 0, err
	}
	packets := make([]packet, 0, len(cues))
	for i, e := range blocks {
		b, err := r.read(e.start, e.end-e.start)
		if err != nil {
			return nil, 0, err
		}
		text, err := readPacket(b, selected.number, cues[i], locations[i].timestamp)
		if err != nil {
			return nil, 0, err
		}
		if strings.TrimSpace(text) != "" {
			packets = append(packets, packet{cues[i].time, cues[i].duration, text})
		}
	}
	if len(packets) == 0 {
		return nil, 0, ErrEmpty
	}
	sort.SliceStable(packets, func(i, j int) bool { return packets[i].time < packets[j].time })
	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	for _, p := range packets {
		start, err := milliseconds(p.time, scale)
		if err != nil {
			return nil, 0, err
		}
		end, err := milliseconds(p.time+p.duration, scale)
		if err != nil || end <= start {
			return nil, 0, ErrIndex
		}
		text := vttText(p.text)
		line := fmt.Sprintf("%s --> %s\n%s\n\n", clock(start), clock(end), text)
		if int64(out.Len())+int64(len(line)) > r.limits.MaxOutputBytes {
			return nil, 0, ErrLimit
		}
		out.WriteString(line)
	}
	return []byte(out.String()), len(packets), nil
}

func locate(r *reader) (element, map[uint64]int64, error) {
	var segment element
	for off, n := int64(0), 0; n < 4 && off < r.source.Size; n++ {
		e, err := r.element(off, r.source.Size)
		if err != nil {
			return segment, nil, err
		}
		if e.id == idSegment {
			segment = e
			break
		}
		if e.unknown {
			return segment, nil, ErrIndex
		}
		off = e.end
	}
	if segment.id == 0 {
		return segment, nil, ErrIndex
	}
	positions := make(map[uint64]int64)
	for off, n := segment.start, 0; off < segment.end && n < 32; n++ {
		e, err := r.element(off, segment.end)
		if err != nil {
			return segment, nil, err
		}
		positions[e.id] = off
		if e.id == idSeekHead {
			if e.unknown {
				return segment, nil, ErrIndex
			}
			if e.end-e.start > 1<<20 {
				return segment, nil, ErrLimit
			}
			b, err := r.read(e.start, e.end-e.start)
			if err != nil {
				return segment, nil, err
			}
			err = children(b, func(id uint64, b []byte) error {
				if id != 0x4dbb {
					return nil
				}
				var target, pos uint64
				var gotTarget, gotPos bool
				if err := children(b, func(id uint64, b []byte) error {
					if id != 0x53ab && id != 0x53ac {
						return nil
					}
					v, err := unsigned(b)
					if err != nil {
						return err
					}
					if id == 0x53ab {
						if gotTarget {
							return ErrIndex
						}
						target = v
						gotTarget = true
					} else {
						if gotPos {
							return ErrIndex
						}
						pos = v
						gotPos = true
					}
					return nil
				}); err != nil {
					return err
				}
				if !gotTarget || !gotPos || pos >= uint64(segment.end-segment.start) {
					return ErrIndex
				}
				p := segment.start + int64(pos)
				if previous, ok := positions[target]; ok && previous != p {
					return ErrIndex
				}
				positions[target] = p
				return nil
			})
			if err != nil {
				return segment, nil, err
			}
		}
		if e.id == idCluster {
			break
		}
		if e.unknown {
			return segment, nil, ErrIndex
		}
		off = e.end
	}
	return segment, positions, nil
}

func selectTrack(b []byte, want Track) (selectedTrack, error) {
	var chosen selectedTrack
	ordinal := 0
	seen := make(map[uint64]bool)
	err := children(b, func(id uint64, b []byte) error {
		if id != 0xae {
			return nil
		}
		index := ordinal
		ordinal++
		if ordinal > 1024 {
			return ErrLimit
		}
		var number, kind uint64
		var codec, language string
		var unsupported bool
		fields := make(map[uint64]bool)
		if err := children(b, func(id uint64, b []byte) error {
			switch id {
			case 0xd7, 0x83, 0x86, 0x22b59c, 0x22b59d:
				if fields[id] {
					return ErrIndex
				}
				fields[id] = true
			}
			switch id {
			case 0xd7:
				var e error
				number, e = unsigned(b)
				return e
			case 0x83:
				var e error
				kind, e = unsigned(b)
				return e
			case 0x86:
				codec = string(b)
			case 0x22b59c:
				if !fields[0x22b59d] {
					language = string(b)
				}
			case 0x22b59d:
				language = string(b)
			case 0x6d80, 0x23314f, 0x537f, 0x56aa:
				unsupported = true // compression, timestamp scaling/offset or codec delay
			}
			return nil
		}); err != nil {
			return err
		}
		if number == 0 || seen[number] || kind == 0 {
			return ErrIndex
		}
		seen[number] = true
		if index == want.Index {
			if kind != 17 || codec != "S_TEXT/UTF8" || unsupported {
				return ErrUnsupported
			}
			if want.Language != "" && language != "" && normalizeLanguage(want.Language) != normalizeLanguage(language) {
				return ErrIndex
			}
			chosen.number = number
		}
		return nil
	})
	if err != nil {
		return selectedTrack{}, err
	}
	if chosen.number == 0 {
		return selectedTrack{}, ErrIndex
	}
	return chosen, nil
}

func readCues(b []byte, number uint64, maxCues int, segmentSize int64, scale uint64) ([]cue, error) {
	var cues []cue
	seen := make(map[[2]uint64]bool)
	points := 0
	err := children(b, func(id uint64, b []byte) error {
		if id != 0xbb {
			return nil
		}
		points++
		if points > 200000 {
			return ErrLimit
		}
		var timestamp uint64
		gotTime := false
		var positions [][]byte
		if err := children(b, func(id uint64, b []byte) error {
			if id == 0xb3 {
				if gotTime {
					return ErrIndex
				}
				var e error
				timestamp, e = unsigned(b)
				gotTime = true
				return e
			}
			if id == 0xb7 {
				positions = append(positions, b)
				if len(positions) > 1024 {
					return ErrLimit
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !gotTime {
			return ErrIndex
		}
		for _, b := range positions {
			values := make(map[uint64]uint64)
			if err := children(b, func(id uint64, b []byte) error {
				if id != 0xf7 && id != 0xf1 && id != 0xf0 && id != 0xb2 {
					return nil
				}
				if _, ok := values[id]; ok {
					return ErrIndex
				}
				v, err := unsigned(b)
				values[id] = v
				return err
			}); err != nil {
				return err
			}
			if values[0xf7] != number {
				continue
			}
			for _, id := range []uint64{0xf1, 0xf0, 0xb2} {
				if _, ok := values[id]; !ok {
					return ErrIndex
				}
			}
			c := cue{values[0xf1], values[0xf0], timestamp, values[0xb2]}
			if c.cluster >= uint64(segmentSize) || c.relative >= uint64(segmentSize) || c.duration == 0 || c.duration > ^uint64(0)-c.time {
				return ErrIndex
			}
			if _, err := milliseconds(c.time+c.duration, scale); err != nil {
				return err
			}
			key := [2]uint64{c.cluster, c.relative}
			if seen[key] {
				return ErrIndex
			}
			seen[key] = true
			cues = append(cues, c)
			if len(cues) > maxCues {
				return ErrLimit
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(cues) == 0 {
		return nil, ErrIndex
	}
	return cues, nil
}

func clusterTime(r *reader, cluster element) (uint64, error) {
	for off, n := cluster.start, 0; off < cluster.end && n < 16; n++ {
		// A timestamp after substantial media payload is not a supported index.
		if off-cluster.start > 4096 {
			return 0, ErrIndex
		}
		e, err := r.element(off, cluster.end)
		if err != nil {
			return 0, err
		}
		if e.unknown {
			return 0, ErrIndex
		}
		if e.id == 0xe7 {
			if e.end-e.start > 8 {
				return 0, ErrIndex
			}
			b, err := r.read(e.start, e.end-e.start)
			if err != nil {
				return 0, err
			}
			return unsigned(b)
		}
		off = e.end
	}
	return 0, ErrIndex
}

func readPacket(b []byte, number uint64, c cue, clusterTime uint64) (string, error) {
	var block []byte
	var duration uint64
	gotDuration := false
	if err := children(b, func(id uint64, b []byte) error {
		switch id {
		case 0xa1:
			if block != nil {
				return ErrIndex
			}
			block = b
		case 0x9b:
			if gotDuration {
				return ErrIndex
			}
			gotDuration = true
			var e error
			duration, e = unsigned(b)
			return e
		case 0xfb, 0x75a1, 0xa4, 0x75a2:
			return ErrUnsupported // references, additions, codec state, discard padding
		}
		return nil
	}); err != nil {
		return "", err
	}
	if !gotDuration || duration != c.duration {
		return "", ErrIndex
	}
	actual, n, err := vint(block, false)
	if err != nil || len(block) < n+3 {
		return "", ErrIndex
	}
	if actual != number {
		return "", ErrIndex
	}
	if block[n+2]&0x06 != 0 {
		return "", ErrUnsupported
	}
	relative := int64(int16(binary.BigEndian.Uint16(block[n : n+2])))
	if clusterTime > uint64(1<<63-1) || int64(clusterTime)+relative < 0 || uint64(int64(clusterTime)+relative) != c.time {
		return "", ErrIndex
	}
	text := string(block[n+3:])
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", ErrUnsupported
	}
	return text, nil
}

func normalizeCodec(codec string) string {
	switch strings.ToLower(codec) {
	case "srt", "subrip", "s_text/utf8":
		return "subrip"
	}
	return strings.ToLower(codec)
}
func normalizeLanguage(language string) string {
	switch strings.ToLower(language) {
	case "en", "eng":
		return "eng"
	case "zh", "chi", "zho":
		return "zho"
	case "ja", "jpn":
		return "jpn"
	case "ko", "kor":
		return "kor"
	case "fr", "fre", "fra":
		return "fra"
	case "de", "ger", "deu":
		return "deu"
	case "es", "spa":
		return "spa"
	case "und", "":
		return "und"
	}
	return strings.ToLower(language)
}
func clock(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
func vttText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	// An empty line terminates a WebVTT cue. Preserve intentional blank lines
	// with a zero-width character instead of allowing payload cue injection.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = "\u200b"
		}
	}
	return strings.Join(lines, "\n")
}
