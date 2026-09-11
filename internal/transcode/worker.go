package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

const maxMetadataBox = 2 << 20

var errWindowReady = errors.New("requested output window is complete")

type runSpec struct {
	Input      string
	Plan       Plan
	Timeline   Timeline
	Start, End int // end is exclusive
	Init       func([]byte, []movieTrack) error
	Writer     func(int) (*cacheWriter, error)
	Commit     func(int, *cacheWriter) error
	Started    func(int)
}

func seconds(ticks int64) string {
	return strconv.FormatFloat(float64(ticks)/float64(TicksPerSecond), 'f', 7, 64)
}

func runFFmpeg(ctx context.Context, path string, spec runSpec) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Seek with preroll so the AAC encoder is established before the requested
	// interval. The source index and output fragments retain absolute timestamps.
	preroll := max(0, spec.Start-1)
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-ss", seconds(spec.Timeline.Cuts[preroll]), "-noaccurate_seek", "-copyts", "-i", spec.Input,
		"-map", fmt.Sprintf("0:%d", spec.Plan.Video.Index), "-map", fmt.Sprintf("0:%d", spec.Plan.Audio.Index), "-map_metadata", "-1", "-map_chapters", "-1", "-c:v", "copy"}
	if spec.Plan.Video.Codec == "hevc" {
		args = append(args, "-bsf:v", "hevc_mp4toannexb,extract_extradata", "-tag:v", "hvc1")
	}
	if spec.Plan.AudioCodec == "copy" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", strconv.Itoa(spec.Plan.AudioChannels), "-ar", "48000", "-b:a", strconv.FormatInt(spec.Plan.AudioBitrate, 10), "-threads:a", "1")
	}
	args = append(args, "-avoid_negative_ts", "disabled")
	if spec.Plan.Container == "ts" {
		args = append(args, "-mpegts_copyts", "1", "-mpegts_flags", "+resend_headers", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", "pipe:1")
	} else {
		args = append(args, "-use_editlist", "0", "-write_btrt", "0", "-movflags", "delay_moov+frag_keyframe+default_base_moof+negative_cts_offsets+frag_discont", "-f", "mp4", "pipe:1")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ErrWorker
	}
	// Error output is deliberately discarded: FFmpeg can echo scoped source
	// URLs. The caller records a finite failure code, never the command or URL.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return ErrWorker
	}
	if spec.Started != nil {
		spec.Started(cmd.Process.Pid)
	}
	var parseErr error
	if spec.Plan.Container == "ts" {
		parseErr = splitTransport(stdout, spec)
	} else {
		parseErr = splitFragments(stdout, spec)
	}
	if parseErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if errors.Is(parseErr, errWindowReady) {
		return nil
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if parseErr != nil {
		return parseErr
	}
	if waitErr != nil {
		return ErrWorker
	}
	return nil
}

func splitFragments(r io.Reader, spec runSpec) error {
	var init []byte
	var tracks []movieTrack
	var current *cacheWriter
	index := -1
	var moof []byte
	defer func() {
		if current != nil {
			current.abort()
		}
	}()
	commit := func() error {
		if current == nil {
			return nil
		}
		if err := spec.Commit(index, current); err != nil {
			return err
		}
		current = nil
		return nil
	}
	for {
		kind, header, n, err := readBoxHeader(r)
		if err == io.EOF {
			if spec.End != spec.Timeline.Len() {
				return ErrWorker
			}
			if index != spec.End-1 {
				return ErrWorker
			}
			return commit()
		}
		if err != nil {
			return ErrWorker
		}
		switch kind {
		case "ftyp", "moov":
			if n > maxMetadataBox || len(init)+len(header)+int(n) > maxMetadataBox {
				return ErrWorker
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(r, b); err != nil {
				return ErrWorker
			}
			init = append(init, header...)
			init = append(init, b...)
			if kind == "moov" {
				tracks, err = movieTracks(b)
				if err != nil {
					return err
				}
				if err = spec.Init(init, tracks); err != nil {
					return err
				}
			}
		case "moof":
			if len(tracks) == 0 || n > maxMetadataBox {
				return ErrWorker
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(r, b); err != nil {
				return ErrWorker
			}
			pts, err := fragmentTime(b, tracks)
			if err != nil {
				return err
			}
			// Container timestamps can be rounded to milliseconds. This tolerance
			// only reconciles the same indexed keyframe, not an arbitrary cut.
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
					current, err = spec.Writer(index)
					if err != nil {
						return err
					}
				}
			}
			moof = append(header, b...)
			// Fragment sequence numbers are stable across regenerated intervals.
			_ = walkBoxes(b, func(box mp4Box) error {
				if box.kind == "mfhd" && len(box.data) >= 8 {
					binary.BigEndian.PutUint32(box.data[4:], uint32(spec.Timeline.Segment(pts)+1))
				}
				return nil
			})
			copy(moof[len(header):], b)
		case "mdat":
			if moof == nil {
				return ErrWorker
			}
			if current == nil {
				if _, err = io.CopyN(io.Discard, r, n); err != nil {
					return ErrWorker
				}
			} else {
				if _, err = current.Write(moof); err != nil {
					return err
				}
				if _, err = current.Write(header); err != nil {
					return err
				}
				if _, err = io.CopyN(current, r, n); err != nil {
					return err
				}
			}
			moof = nil
		case "mfra", "free", "sidx", "styp":
			if n > maxMetadataBox {
				return ErrWorker
			}
			if _, err = io.CopyN(io.Discard, r, n); err != nil {
				return ErrWorker
			}
		default:
			return ErrWorker
		}
	}
}

func sameTracks(a, b []movieTrack) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Scale != b[i].Scale || a[i].Video != b[i].Video || !bytes.Equal(a[i].Codec, b[i].Codec) {
			return false
		}
	}
	return true
}
