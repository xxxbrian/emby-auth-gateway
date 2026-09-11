package transcode

import (
	"encoding/binary"
	"io"
	"math"
)

type mp4Box struct {
	kind string
	data []byte
}

func walkBoxes(b []byte, visit func(mp4Box) error) error {
	for off := 0; off < len(b); {
		if len(b)-off < 8 {
			return ErrIndex
		}
		size := uint64(binary.BigEndian.Uint32(b[off:]))
		header := 8
		if size == 1 {
			if len(b)-off < 16 {
				return ErrIndex
			}
			size = binary.BigEndian.Uint64(b[off+8:])
			header = 16
		} else if size == 0 {
			size = uint64(len(b) - off)
		}
		if size < uint64(header) || size > uint64(len(b)-off) {
			return ErrIndex
		}
		if err := visit(mp4Box{kind: string(b[off+4 : off+8]), data: b[off+header : off+int(size)]}); err != nil {
			return err
		}
		off += int(size)
	}
	return nil
}

func child(b []byte, kind string) ([]byte, error) {
	var result []byte
	err := walkBoxes(b, func(box mp4Box) error {
		if box.kind == kind {
			if result != nil {
				return ErrIndex
			}
			result = box.data
		}
		return nil
	})
	if err != nil || result == nil {
		return nil, ErrIndex
	}
	return result, nil
}
func descendants(b []byte, path ...string) ([]byte, error) {
	var err error
	for _, p := range path {
		b, err = child(b, p)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}
func clockScale(b []byte) (uint32, error) {
	if len(b) < 16 {
		return 0, ErrIndex
	}
	off := 12
	if b[0] == 1 {
		off = 20
	} else if b[0] != 0 {
		return 0, ErrIndex
	}
	if len(b) < off+4 {
		return 0, ErrIndex
	}
	v := binary.BigEndian.Uint32(b[off:])
	if v == 0 {
		return 0, ErrIndex
	}
	return v, nil
}

func mp4Timeline(r *indexReader) (Timeline, error) {
	var moov []byte
	for offset, n := int64(0), 0; offset < r.source.Media.Size && n < 256; n++ {
		b, err := r.read(offset, int(min(16, r.source.Media.Size-offset)))
		if err != nil || len(b) < 8 {
			return Timeline{}, ErrIndex
		}
		size := uint64(binary.BigEndian.Uint32(b))
		header := int64(8)
		if size == 1 {
			if len(b) < 16 {
				return Timeline{}, ErrIndex
			}
			size = binary.BigEndian.Uint64(b[8:])
			header = 16
		} else if size == 0 {
			size = uint64(r.source.Media.Size - offset)
		}
		if size < uint64(header) || size > uint64(r.source.Media.Size-offset) {
			return Timeline{}, ErrIndex
		}
		if string(b[4:8]) == "moov" {
			moov, err = r.read(offset+header, int(size)-int(header))
			if err != nil {
				return Timeline{}, err
			}
			break
		}
		offset += int64(size)
	}
	if moov == nil {
		return Timeline{}, ErrIndex
	}
	mvhd, err := child(moov, "mvhd")
	if err != nil {
		return Timeline{}, err
	}
	movieScale, err := clockScale(mvhd)
	if err != nil {
		return Timeline{}, err
	}
	var video []byte
	err = walkBoxes(moov, func(box mp4Box) error {
		if box.kind != "trak" {
			return nil
		}
		handler, err := descendants(box.data, "mdia", "hdlr")
		if err != nil || len(handler) < 12 {
			return ErrIndex
		}
		if string(handler[8:12]) == "vide" {
			if video != nil {
				return ErrIndex
			}
			video = box.data
		}
		return nil
	})
	if err != nil || video == nil {
		return Timeline{}, ErrIndex
	}
	mdhd, err := descendants(video, "mdia", "mdhd")
	if err != nil {
		return Timeline{}, err
	}
	scale, err := clockScale(mdhd)
	if err != nil {
		return Timeline{}, err
	}
	stbl, err := descendants(video, "mdia", "minf", "stbl")
	if err != nil {
		return Timeline{}, err
	}
	stts, err := child(stbl, "stts")
	if err != nil {
		return Timeline{}, err
	}
	durations, err := sampleRuns(stts, false)
	if err != nil {
		return Timeline{}, err
	}
	offsets := []sampleRun{{count: math.MaxUint32, value: 0}}
	if ctts, e := child(stbl, "ctts"); e == nil {
		offsets, err = sampleRuns(ctts, ctts[0] == 1)
		if err != nil {
			return Timeline{}, err
		}
	}
	var sampleCount uint64
	for _, run := range durations {
		sampleCount += uint64(run.count)
	}
	if sampleCount == 0 || sampleCount > 2_000_000 {
		return Timeline{}, ErrIndex
	}
	keys := map[uint32]bool{}
	if stss, e := child(stbl, "stss"); e == nil {
		if len(stss) < 8 {
			return Timeline{}, ErrIndex
		}
		n := binary.BigEndian.Uint32(stss[4:])
		if n > maxKeyframes || uint64(n)*4 > uint64(len(stss)-8) {
			return Timeline{}, ErrIndex
		}
		for i := uint32(0); i < n; i++ {
			sample := binary.BigEndian.Uint32(stss[8+i*4:])
			if sample == 0 || uint64(sample) > sampleCount {
				return Timeline{}, ErrIndex
			}
			keys[sample] = true
		}
	} else {
		if sampleCount > maxKeyframes {
			return Timeline{}, ErrIndex
		}
		for i := uint32(1); uint64(i) <= sampleCount; i++ {
			keys[i] = true
		}
	}
	shift, err := editShift(video, movieScale, scale)
	if err != nil {
		return Timeline{}, err
	}
	var times []int64
	var dts int64
	sample := uint32(1)
	ci := 0
	remaining := offsets[0].count
	for _, run := range durations {
		if run.value <= 0 {
			return Timeline{}, ErrIndex
		}
		for n := uint32(0); n < run.count; n++ {
			if keys[sample] {
				pts := dts + offsets[ci].value
				seconds := float64(pts) / float64(scale)
				if math.Abs(seconds) > 72*3600 {
					return Timeline{}, ErrIndex
				}
				times = append(times, int64(seconds*float64(TicksPerSecond))+shift)
			}
			dts += run.value
			sample++
			remaining--
			if remaining == 0 && uint64(sample) <= sampleCount {
				ci++
				if ci >= len(offsets) {
					return Timeline{}, ErrIndex
				}
				remaining = offsets[ci].count
			}
		}
	}
	return makeTimeline(times, r.source.Media.RunTimeTicks)
}

type sampleRun struct {
	count uint32
	value int64
}

func sampleRuns(b []byte, signed bool) ([]sampleRun, error) {
	if len(b) < 8 {
		return nil, ErrIndex
	}
	n := binary.BigEndian.Uint32(b[4:])
	if n == 0 || n > 2_000_000 || uint64(n)*8 > uint64(len(b)-8) {
		return nil, ErrIndex
	}
	r := make([]sampleRun, n)
	for i := range r {
		count := binary.BigEndian.Uint32(b[8+i*8:])
		raw := binary.BigEndian.Uint32(b[12+i*8:])
		v := int64(raw)
		if signed {
			v = int64(int32(raw))
		}
		if count == 0 {
			return nil, ErrIndex
		}
		r[i] = sampleRun{count, v}
	}
	return r, nil
}
func editShift(trak []byte, movieScale, scale uint32) (int64, error) {
	b, err := descendants(trak, "edts", "elst")
	if err != nil {
		return 0, nil
	}
	if len(b) < 8 {
		return 0, ErrIndex
	}
	count := binary.BigEndian.Uint32(b[4:])
	if count == 0 || count > 2 {
		return 0, ErrIndex
	}
	entry := 12
	if b[0] == 1 {
		entry = 20
	} else if b[0] != 0 {
		return 0, ErrIndex
	}
	if len(b) < 8+int(count)*entry {
		return 0, ErrIndex
	}
	var empty uint64
	var media int64
	for i := 0; i < int(count); i++ {
		d := b[8+i*entry:]
		duration := uint64(binary.BigEndian.Uint32(d))
		m := int64(int32(binary.BigEndian.Uint32(d[4:])))
		if entry == 20 {
			duration = binary.BigEndian.Uint64(d)
			m = int64(binary.BigEndian.Uint64(d[8:]))
		}
		if binary.BigEndian.Uint32(d[entry-4:]) != 0x10000 {
			return 0, ErrIndex
		}
		if m == -1 && i == 0 {
			empty = duration
		} else if m >= 0 {
			media = m
		} else {
			return 0, ErrIndex
		}
	}
	shift := float64(empty)/float64(movieScale) - float64(media)/float64(scale)
	if math.Abs(shift) > 72*3600 {
		return 0, ErrIndex
	}
	return int64(shift * float64(TicksPerSecond)), nil
}

// readBoxHeader leaves the payload in r so media data can be quota-checked and
// copied directly to disk without allocating a media-sized Go byte slice.
func readBoxHeader(r io.Reader) (string, []byte, int64, error) {
	header := make([]byte, 8)
	_, err := io.ReadFull(r, header)
	if err != nil {
		return "", nil, 0, err
	}
	n := uint64(binary.BigEndian.Uint32(header))
	if n == 1 {
		header = append(header, make([]byte, 8)...)
		if _, err = io.ReadFull(r, header[8:]); err != nil {
			return "", nil, 0, err
		}
		n = binary.BigEndian.Uint64(header[8:])
	}
	if n < uint64(len(header)) || n > math.MaxInt64 {
		return "", nil, 0, ErrWorker
	}
	return string(header[4:8]), header, int64(n) - int64(len(header)), nil
}

type movieTrack struct {
	ID, Scale uint32
	Codec     []byte
	Video     bool
}

func movieTracks(moov []byte) ([]movieTrack, error) {
	var tracks []movieTrack
	err := walkBoxes(moov, func(box mp4Box) error {
		if box.kind != "trak" {
			return nil
		}
		tkhd, e := child(box.data, "tkhd")
		if e != nil || len(tkhd) < 16 {
			return ErrWorker
		}
		off := 12
		if tkhd[0] == 1 {
			off = 20
		}
		if len(tkhd) < off+4 {
			return ErrWorker
		}
		id := binary.BigEndian.Uint32(tkhd[off:])
		mdhd, e := descendants(box.data, "mdia", "mdhd")
		if e != nil {
			return ErrWorker
		}
		scale, e := clockScale(mdhd)
		if e != nil {
			return ErrWorker
		}
		handler, e := descendants(box.data, "mdia", "hdlr")
		if e != nil || len(handler) < 12 {
			return ErrWorker
		}
		codec, e := descendants(box.data, "mdia", "minf", "stbl", "stsd")
		if e != nil {
			return ErrWorker
		}
		tracks = append(tracks, movieTrack{ID: id, Scale: scale, Codec: append([]byte(nil), codec...), Video: string(handler[8:12]) == "vide"})
		return nil
	})
	if err != nil || len(tracks) != 2 {
		return nil, ErrWorker
	}
	return tracks, nil
}

func fragmentTime(moof []byte, tracks []movieTrack) (int64, error) {
	var video movieTrack
	for _, t := range tracks {
		if t.Video {
			video = t
		}
	}
	if video.Scale == 0 {
		return 0, ErrWorker
	}
	var pts int64
	found := false
	err := walkBoxes(moof, func(box mp4Box) error {
		if box.kind != "traf" {
			return nil
		}
		tfhd, e := child(box.data, "tfhd")
		if e != nil || len(tfhd) < 8 {
			return ErrWorker
		}
		if binary.BigEndian.Uint32(tfhd[4:]) != video.ID {
			return nil
		}
		tfdt, e := child(box.data, "tfdt")
		if e != nil || len(tfdt) < 8 {
			return ErrWorker
		}
		base := uint64(binary.BigEndian.Uint32(tfdt[4:]))
		if tfdt[0] == 1 {
			if len(tfdt) < 12 {
				return ErrWorker
			}
			base = binary.BigEndian.Uint64(tfdt[4:])
		}
		if base > uint64(72*3600)*uint64(video.Scale) {
			return ErrWorker
		}
		trun, e := child(box.data, "trun")
		if e != nil || len(trun) < 8 || binary.BigEndian.Uint32(trun[4:]) == 0 {
			return ErrWorker
		}
		flags := binary.BigEndian.Uint32(trun) & 0xffffff
		off := 8
		if flags&1 != 0 {
			off += 4
		}
		if flags&4 != 0 {
			off += 4
		}
		for _, flag := range []uint32{0x100, 0x200, 0x400} {
			if flags&flag != 0 {
				off += 4
			}
		}
		var cts int64
		if flags&0x800 != 0 {
			if len(trun) < off+4 {
				return ErrWorker
			}
			raw := binary.BigEndian.Uint32(trun[off:])
			cts = int64(raw)
			if trun[0] == 1 {
				cts = int64(int32(raw))
			}
		}
		pts = int64((float64(base) + float64(cts)) * float64(TicksPerSecond) / float64(video.Scale))
		found = true
		return nil
	})
	if err != nil || !found {
		return 0, ErrWorker
	}
	return pts, nil
}
