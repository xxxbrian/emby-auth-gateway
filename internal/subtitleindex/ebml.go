package subtitleindex

import "math"

type element struct {
	id         uint64
	start, end int64
	unknown    bool
}

func vint(b []byte, keepMarker bool) (uint64, int, error) {
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
	if len(b) < n || (keepMarker && n > 4) {
		return 0, 0, ErrIndex
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= (1 << uint(8-n)) - 1
	}
	for _, x := range b[1:n] {
		v = v<<8 | uint64(x)
	}
	return v, n, nil
}

func (r *reader) element(offset, boundary int64) (element, error) {
	if offset < 0 || offset >= boundary || boundary > r.source.Size {
		return element{}, ErrIndex
	}
	b, err := r.read(offset, min(12, boundary-offset))
	if err != nil {
		return element{}, err
	}
	id, n, err := vint(b, true)
	if err != nil {
		return element{}, err
	}
	size, m, err := vint(b[n:], false)
	if err != nil {
		return element{}, err
	}
	start := offset + int64(n+m)
	unknown := size == (uint64(1)<<uint(7*m))-1
	if unknown {
		size = uint64(boundary - start)
	}
	if size > uint64(boundary-start) {
		return element{}, ErrIndex
	}
	return element{id: id, start: start, end: start + int64(size), unknown: unknown}, nil
}

func children(b []byte, visit func(uint64, []byte) error) error {
	for off := 0; off < len(b); {
		id, n, err := vint(b[off:], true)
		if err != nil {
			return err
		}
		size, m, err := vint(b[off+n:], false)
		if err != nil {
			return err
		}
		start := off + n + m
		if size == (uint64(1)<<uint(7*m))-1 || size > uint64(len(b)-start) {
			return ErrIndex
		}
		end := start + int(size)
		if err := visit(id, b[start:end]); err != nil {
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
	var n uint64
	for _, x := range b {
		n = n<<8 | uint64(x)
	}
	return n, nil
}

func milliseconds(ticks, scale uint64) (int64, error) {
	if scale == 0 || scale > 1_000_000_000 || ticks > math.MaxInt64/scale {
		return 0, ErrIndex
	}
	n := int64(ticks * scale / 1_000_000)
	if n > 72*3600*1000 {
		return 0, ErrIndex
	}
	return n, nil
}
