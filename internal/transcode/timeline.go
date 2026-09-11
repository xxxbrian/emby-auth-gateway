package transcode

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const (
	maxIndexBytes    = 24 << 20
	maxKeyframes     = 200_000
	maxDurationTicks = 72 * 3600 * TicksPerSecond
	segmentTarget    = 4 * TicksPerSecond
)

type Timeline struct {
	Cuts []int64 // original presentation timestamps; first boundary is zero
	Keys []int64
}

func (t Timeline) Len() int { return len(t.Cuts) - 1 }
func (t Timeline) Segment(ticks int64) int {
	return max(0, min(t.Len()-1, sort.Search(len(t.Cuts), func(i int) bool { return t.Cuts[i] > ticks })-1))
}

func makeTimeline(keys []int64, duration int64) (Timeline, error) {
	if len(keys) == 0 || len(keys) > maxKeyframes || duration <= 0 || duration > maxDurationTicks {
		return Timeline{}, ErrIndex
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	unique := make([]int64, 0, len(keys))
	for _, k := range keys {
		if k >= 0 && k < duration && (len(unique) == 0 || k > unique[len(unique)-1]) {
			unique = append(unique, k)
		}
	}
	if len(unique) == 0 || unique[0] > TicksPerSecond {
		return Timeline{}, ErrIndex
	}
	cuts := []int64{0}
	next := segmentTarget
	for _, k := range unique {
		if k >= next {
			cuts = append(cuts, k)
			next = k + segmentTarget
		}
	}
	if duration-cuts[len(cuts)-1] < TicksPerSecond/10 && len(cuts) > 1 {
		cuts = cuts[:len(cuts)-1]
	}
	cuts = append(cuts, duration)
	return Timeline{Cuts: cuts, Keys: unique}, nil
}

type indexReader struct {
	ctx    context.Context
	source Source
	used   int64
	blocks map[int64][]byte
}

func (r *indexReader) read(offset int64, n int) ([]byte, error) {
	if offset < 0 || n < 0 || int64(n) > r.source.Media.Size-offset || n > maxIndexBytes {
		return nil, ErrIndex
	}
	if n <= 4096 {
		start := offset / 4096 * 4096
		if offset+int64(n) <= start+4096 {
			if b := r.blocks[start]; b != nil {
				return b[offset-start : offset-start+int64(n)], nil
			}
			b, err := r.fetch(start, int(min(4096, r.source.Media.Size-start)))
			if err != nil {
				return nil, err
			}
			r.blocks[start] = b
			return b[offset-start : offset-start+int64(n)], nil
		}
	}
	return r.fetch(offset, n)
}
func (r *indexReader) fetch(offset int64, n int) ([]byte, error) {
	if r.used+int64(n) > maxIndexBytes {
		return nil, ErrIndex
	}
	r.used += int64(n)
	body, err := r.source.Open(r.ctx, offset, int64(n))
	if err != nil {
		return nil, ErrSource
	}
	defer body.Close()
	b := make([]byte, n)
	if _, err = io.ReadFull(body, b); err != nil {
		return nil, ErrSource
	}
	return b, nil
}

func ReadTimeline(ctx context.Context, source Source) (Timeline, error) {
	if source.Open == nil || source.Media.Size <= 0 {
		return Timeline{}, ErrSource
	}
	r := &indexReader{ctx: ctx, source: source, blocks: make(map[int64][]byte)}
	switch strings.ToLower(source.Media.Container) {
	case "mkv":
		return matroskaTimeline(r)
	case "mp4", "mov":
		return mp4Timeline(r)
	default:
		return Timeline{}, ErrIndex
	}
}

func ebmlVint(b []byte, keep bool) (uint64, int, error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, ErrIndex
	}
	n := 1
	for b[0]&(1<<uint(8-n)) == 0 {
		n++
		if n > 8 {
			return 0, 0, ErrIndex
		}
	}
	if len(b) < n {
		return 0, 0, io.ErrUnexpectedEOF
	}
	v := uint64(b[0])
	if !keep {
		v &= uint64((1 << uint(8-n)) - 1)
	}
	for _, x := range b[1:n] {
		v = v<<8 | uint64(x)
	}
	return v, n, nil
}

type element struct {
	id         uint64
	start, end int64
}

func (r *indexReader) ebml(offset int64) (element, error) {
	b, err := r.read(offset, int(min(16, r.source.Media.Size-offset)))
	if err != nil {
		return element{}, err
	}
	id, n, err := ebmlVint(b, true)
	if err != nil {
		return element{}, err
	}
	size, m, err := ebmlVint(b[n:], false)
	if err != nil {
		return element{}, err
	}
	start := offset + int64(n+m)
	if size == uint64(1)<<uint(7*m)-1 {
		size = uint64(r.source.Media.Size - start)
	}
	if size > uint64(r.source.Media.Size-start) {
		return element{}, ErrIndex
	}
	return element{id: id, start: start, end: start + int64(size)}, nil
}
func elements(b []byte, visit func(uint64, []byte) error) error {
	for off := 0; off < len(b); {
		id, n, err := ebmlVint(b[off:], true)
		if err != nil {
			return err
		}
		size, m, err := ebmlVint(b[off+n:], false)
		if err != nil {
			return err
		}
		start := off + n + m
		if size > uint64(len(b)-start) {
			return ErrIndex
		}
		end := start + int(size)
		if err = visit(id, b[start:end]); err != nil {
			return err
		}
		off = end
	}
	return nil
}
func unsigned(b []byte) (uint64, error) {
	if len(b) == 0 || len(b) > 8 {
		return 0, ErrIndex
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, nil
}

func matroskaTimeline(r *indexReader) (Timeline, error) {
	var segment element
	for offset, n := int64(0), 0; n < 4; n++ {
		e, err := r.ebml(offset)
		if err != nil {
			return Timeline{}, err
		}
		if e.id == 0x18538067 {
			segment = e
			break
		}
		offset = e.end
	}
	if segment.id == 0 {
		return Timeline{}, ErrIndex
	}
	positions := map[uint64]int64{}
	for offset, n := segment.start, 0; offset < segment.end && n < 32; n++ {
		e, err := r.ebml(offset)
		if err != nil {
			return Timeline{}, err
		}
		positions[e.id] = offset
		if e.id == 0x114d9b74 {
			b, err := r.read(e.start, int(e.end-e.start))
			if err != nil {
				return Timeline{}, err
			}
			err = elements(b, func(id uint64, b []byte) error {
				if id != 0x4dbb {
					return nil
				}
				var target, pos uint64
				if err := elements(b, func(id uint64, b []byte) error {
					v, err := unsigned(b)
					if id == 0x53ab {
						target = v
					}
					if id == 0x53ac {
						pos = v
					}
					return err
				}); err != nil {
					return err
				}
				if pos > uint64(segment.end-segment.start) {
					return ErrIndex
				}
				positions[target] = segment.start + int64(pos)
				return nil
			})
			if err != nil {
				return Timeline{}, err
			}
		}
		if e.id == 0x1f43b675 {
			break
		}
		offset = e.end
	}
	load := func(id uint64) ([]byte, error) {
		p, ok := positions[id]
		if !ok {
			return nil, ErrIndex
		}
		e, err := r.ebml(p)
		if err != nil || e.id != id {
			return nil, ErrIndex
		}
		return r.read(e.start, int(e.end-e.start))
	}
	info, err := load(0x1549a966)
	if err != nil {
		return Timeline{}, err
	}
	scale := uint64(1_000_000)
	duration := float64(0)
	if err = elements(info, func(id uint64, b []byte) error {
		switch id {
		case 0x2ad7b1:
			var e error
			scale, e = unsigned(b)
			return e
		case 0x4489:
			if len(b) == 4 {
				duration = float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
			} else if len(b) == 8 {
				duration = math.Float64frombits(binary.BigEndian.Uint64(b))
			} else {
				return ErrIndex
			}
		}
		return nil
	}); err != nil || scale == 0 || scale > 1_000_000_000 {
		return Timeline{}, ErrIndex
	}
	durationTicks := r.source.Media.RunTimeTicks
	if duration > 0 && !math.IsInf(duration, 0) && !math.IsNaN(duration) && duration*float64(scale)/100 <= float64(maxDurationTicks) {
		durationTicks = int64(duration * float64(scale) / 100)
	}
	tracks, err := load(0x1654ae6b)
	if err != nil {
		return Timeline{}, err
	}
	videoTrack := uint64(0)
	if err = elements(tracks, func(id uint64, b []byte) error {
		if id != 0xae {
			return nil
		}
		var number, kind uint64
		var codec string
		err := elements(b, func(id uint64, b []byte) error {
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
			}
			return nil
		})
		if kind == 1 {
			if videoTrack != 0 || codec != "V_MPEG4/ISO/AVC" && codec != "V_MPEGH/ISO/HEVC" {
				return ErrIndex
			}
			videoTrack = number
		}
		return err
	}); err != nil || videoTrack == 0 {
		return Timeline{}, ErrIndex
	}
	cues, err := load(0x1c53bb6b)
	if err != nil {
		return Timeline{}, err
	}
	keys := make([]int64, 0, 2048)
	err = elements(cues, func(id uint64, b []byte) error {
		if id != 0xbb {
			return nil
		}
		var timecode uint64
		selected := false
		err := elements(b, func(id uint64, b []byte) error {
			if id == 0xb3 {
				var e error
				timecode, e = unsigned(b)
				return e
			}
			if id != 0xb7 {
				return nil
			}
			var track, pos uint64
			random := true
			err := elements(b, func(id uint64, b []byte) error {
				if id == 0xdb {
					random = false
					return nil
				}
				if id != 0xf7 && id != 0xf1 && id != 0xea {
					return nil
				}
				v, e := unsigned(b)
				if id == 0xf7 {
					track = v
				}
				if id == 0xf1 {
					pos = v
				}
				if id == 0xea && v != 0 {
					random = false
				}
				return e
			})
			if track == videoTrack && random {
				if pos >= uint64(segment.end-segment.start) {
					return ErrIndex
				}
				selected = true
			}
			return err
		})
		if err != nil {
			return err
		}
		if selected {
			if timecode > uint64(maxDurationTicks)*100/scale || len(keys) >= maxKeyframes {
				return ErrIndex
			}
			keys = append(keys, int64(timecode*scale/100))
		}
		return nil
	})
	if err != nil {
		return Timeline{}, fmt.Errorf("%w: matroska cues", ErrIndex)
	}
	return makeTimeline(keys, durationTicks)
}
