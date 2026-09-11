package transcode

import (
	"io"
)

// splitTransport packages FFmpeg's single-program transport stream at the same
// indexed video random-access points used by the fragmented-MP4 packager.
// A fixed small staging buffer batches disk writes through the shared quota.
func splitTransport(r io.Reader, spec runSpec) error {
	var packet [188]byte
	var pat, pmt []byte
	pmtPID, videoPID := -1, -1
	index := -1
	var current *cacheWriter
	buffer := make([]byte, 0, 64<<10)
	defer func() {
		if current != nil {
			current.abort()
		}
	}()
	flush := func() error {
		if len(buffer) > 0 && current != nil {
			if _, err := current.Write(buffer); err != nil {
				return err
			}
		}
		buffer = buffer[:0]
		return nil
	}
	commit := func() error {
		if current == nil {
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		if err := spec.Commit(index, current); err != nil {
			return err
		}
		current = nil
		return nil
	}
	for {
		_, err := io.ReadFull(r, packet[:])
		if err == io.EOF {
			if spec.End != spec.Timeline.Len() || index != spec.End-1 {
				return ErrWorker
			}
			return commit()
		}
		if err != nil || packet[0] != 0x47 || packet[1]&0x80 != 0 {
			return ErrWorker
		}
		pid := int(packet[1]&0x1f)<<8 | int(packet[2])
		start := packet[1]&0x40 != 0
		control := (packet[3] >> 4) & 3
		offset := 4
		randomAccess := false
		if control&2 != 0 {
			n := int(packet[4])
			if n > 183 {
				return ErrWorker
			}
			if n > 0 {
				randomAccess = packet[5]&0x40 != 0
			}
			offset += 1 + n
		}
		if control&1 != 0 && offset < len(packet) {
			payload := packet[offset:]
			if start && (pid == 0 || pid == pmtPID) {
				pointer := int(payload[0])
				if pointer+4 >= len(payload) {
					return ErrWorker
				}
				section := payload[1+pointer:]
				length := (int(section[1]&15)<<8 | int(section[2])) + 3
				if length > len(section) {
					return ErrWorker
				}
				section = section[:length]
				if pid == 0 && section[0] == 0 {
					if len(section) < 12 {
						return ErrWorker
					}
					for p := 8; p+4 <= len(section)-4; p += 4 {
						if section[p] != 0 || section[p+1] != 0 {
							pmtPID = int(section[p+2]&31)<<8 | int(section[p+3])
							break
						}
					}
					pat = append(pat[:0], packet[:]...)
				} else if pid == pmtPID && section[0] == 2 {
					if len(section) < 16 {
						return ErrWorker
					}
					p := 12 + (int(section[10]&15)<<8 | int(section[11]))
					for p+5 <= len(section)-4 {
						kind := section[p]
						if kind == 0x1b && spec.Plan.Video.Codec == "h264" || kind == 0x24 && spec.Plan.Video.Codec == "hevc" {
							videoPID = int(section[p+1]&31)<<8 | int(section[p+2])
						}
						p += 5 + (int(section[p+3]&15)<<8 | int(section[p+4]))
					}
					pmt = append(pmt[:0], packet[:]...)
				}
			}
			if pid == videoPID && start && randomAccess {
				if len(payload) < 14 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 || payload[7]&0x80 == 0 {
					return ErrWorker
				}
				b := payload[9:14]
				pts90 := int64(b[0]&14)<<29 | int64(b[1])<<22 | int64(b[2]&254)<<14 | int64(b[3])<<7 | int64(b[4]>>1)
				anchor := spec.Timeline.Cuts[max(0, spec.Start-1)] * 90000 / TicksPerSecond
				const wrap = int64(1) << 33
				for pts90 < anchor-wrap/2 {
					pts90 += wrap
				}
				for pts90 > anchor+wrap/2 {
					pts90 -= wrap
				}
				pts := pts90 * TicksPerSecond / 90000
				next := spec.Timeline.Segment(pts + TicksPerSecond/1000)
				if index >= 0 && next < index {
					return ErrWorker
				}
				if next != index {
					if next >= spec.Start && next > 0 && absTicks(pts-spec.Timeline.Cuts[next]) > TicksPerSecond/1000 {
						return ErrIndex
					}
					if err = commit(); err != nil {
						return err
					}
					if next >= spec.End {
						return errWindowReady
					}
					index = next
					if index >= spec.Start {
						if len(pat) == 0 || len(pmt) == 0 {
							return ErrWorker
						}
						current, err = spec.Writer(index)
						if err != nil {
							return err
						}
						buffer = append(buffer, pat...)
						buffer = append(buffer, pmt...)
					}
				}
			}
		}
		if current != nil {
			buffer = append(buffer, packet[:]...)
			if len(buffer) >= 64<<10 {
				if err = flush(); err != nil {
					return err
				}
			}
		}
	}
}

func absTicks(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
