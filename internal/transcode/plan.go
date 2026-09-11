package transcode

import (
	"strconv"
	"strings"
)

func includes(list, value string) bool {
	for _, v := range strings.Split(strings.ToLower(list), ",") {
		if strings.TrimSpace(v) == strings.ToLower(value) {
			return true
		}
	}
	return false
}

func allowed(v *bool) bool { return v == nil || *v }

// Choose returns nil for a compatible direct/upstream path. A non-nil plan
// always copies video; unsupported video cannot accidentally select an encoder.
func Choose(source MediaSource, req Request) (*Plan, error) {
	var video, audio *Stream
	for i := range source.MediaStreams {
		s := &source.MediaStreams[i]
		if s.Type == "Video" && video == nil {
			video = s
		}
		if s.Type != "Audio" || s.IsExternal {
			continue
		}
		if req.AudioStreamIndex != nil {
			if s.Index == *req.AudioStreamIndex {
				audio = s
			}
		} else if audio == nil || source.DefaultAudioStreamIndex != nil && s.Index == *source.DefaultAudioStreamIndex {
			audio = s
		}
	}
	if video == nil || audio == nil || len(req.Profile.DirectPlayProfiles)+len(req.Profile.TranscodingProfiles) == 0 {
		return nil, ErrUnsupported
	}
	bitrate := source.Bitrate
	if bitrate <= 0 {
		bitrate = video.BitRate + audio.BitRate
	}
	maxBitrate := minPositive(req.MaxStreamingBitrate, req.Profile.MaxStreamingBitrate)
	bandwidthOK := maxBitrate <= 0 || bitrate > 0 && bitrate <= maxBitrate
	if allowed(req.EnableDirectPlay) || allowed(req.EnableDirectStream) {
		for _, p := range req.Profile.DirectPlayProfiles {
			if p.Type != "Video" || !matches(p.Container, source.Container) || !matches(p.VideoCodec, video.Codec) || !matches(p.AudioCodec, audio.Codec) {
				continue
			}
			if bandwidthOK && compatible(req.Profile, source, *video, "Video", source.Container) && compatible(req.Profile, source, *audio, "VideoAudio", source.Container) && allowed(req.AllowAudioStreamCopy) && allowed(req.AllowVideoStreamCopy) {
				return nil, nil
			}
		}
	}
	if source.SupportsTranscoding && source.TranscodingURL != "" && allowed(req.EnableTranscoding) {
		return nil, nil
	}
	if !allowed(req.EnableTranscoding) || !allowed(req.AllowVideoStreamCopy) || !includes("hevc,h264", video.Codec) || !includes("mkv,mp4,mov", source.Container) || source.Size <= 0 || source.RunTimeTicks <= 0 {
		return nil, ErrUnsupported
	}
	subtitle := req.SubtitleStreamIndex
	if subtitle == nil {
		subtitle = source.DefaultSubtitleStreamIndex
	}
	if subtitle != nil && *subtitle >= 0 {
		found := false
		for _, stream := range source.MediaStreams {
			if stream.Type == "Subtitle" && stream.Index == *subtitle {
				// Audio conversion preserves external subtitle delivery. Embedded
				// or burned-in subtitles need another media path, never silent loss.
				found = stream.DeliveryMethod == "External" && stream.DeliveryURL != ""
				break
			}
		}
		if !found {
			return nil, ErrUnsupported
		}
	}
	for _, p := range req.Profile.TranscodingProfiles {
		if p.Type != "Video" || p.Context != "" && p.Context != "Streaming" || p.Protocol != "hls" || !matches(p.VideoCodec, video.Codec) {
			continue
		}
		container := "mp4"
		if !includes(p.Container, "m4s") && !includes(p.Container, "mp4") {
			if !includes(p.Container, "ts") {
				continue
			}
			container = "ts"
		}
		outputSource := source
		outputVideo, outputAudio := *video, *audio
		outputVideo.Index, outputAudio.Index = 0, 1
		defaultIndex := 1
		outputSource.DefaultAudioStreamIndex = &defaultIndex
		outputSource.MediaStreams = []Stream{outputVideo, outputAudio}
		if !compatible(req.Profile, outputSource, outputVideo, "Video", container) {
			continue
		}
		channels := audio.Channels
		if channels <= 0 {
			continue
		}
		if n, err := strconv.Atoi(p.MaxAudioChannels); err == nil && n > 0 {
			channels = min(channels, n)
		}
		if req.MaxAudioChannels > 0 {
			channels = min(channels, req.MaxAudioChannels)
		}
		if channels > 6 {
			channels = 6
		}
		// Preserve mono/stereo/5.1 layouts; do not invent an unsupported layout.
		if channels > 2 && channels < 6 {
			channels = 2
		}
		plan := &Plan{Container: container, Video: *video, Audio: *audio, AudioCodec: "copy", AudioChannels: audio.Channels, Reason: "remux_required"}
		if !allowed(req.AllowAudioStreamCopy) || !matches(p.AudioCodec, audio.Codec) || channels != audio.Channels || !compatible(req.Profile, outputSource, outputAudio, "VideoAudio", container) {
			if !includes(p.AudioCodec, "aac") {
				continue
			}
			plan.AudioCodec, plan.AudioChannels, plan.Reason = "aac", channels, "audio_not_supported"
			plan.AudioBitrate = 192_000
			if channels > 2 {
				plan.AudioBitrate = 384_000
			}
			output := outputAudio
			output.Codec, output.Profile, output.Channels, output.BitRate, output.SampleRate = "aac", "LC", channels, plan.AudioBitrate, 48000
			outputSource.MediaStreams[1] = output
			if !compatible(req.Profile, outputSource, output, "VideoAudio", container) {
				continue
			}
		}
		outputBitrate := bitrate
		if plan.AudioCodec != "copy" && audio.BitRate > 0 {
			outputBitrate += plan.AudioBitrate - audio.BitRate
		}
		if maxBitrate > 0 && (outputBitrate <= 0 || outputBitrate > maxBitrate) {
			continue
		}
		return plan, nil
	}
	return nil, ErrUnsupported
}

func minPositive(a, b int64) int64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	return min(a, b)
}

func matches(list, value string) bool { return list == "" || includes(list, value) }

func compatible(profile Profile, source MediaSource, stream Stream, kind, container string) bool {
	for _, p := range profile.ContainerProfiles {
		if p.Type == "Video" && matches(p.Container, container) && !conditionsMatch(p.Conditions, source, stream, false) {
			return false
		}
	}
	for _, p := range profile.CodecProfiles {
		if p.Type != kind || !matches(p.Codec, stream.Codec) || !matches(p.Container, container) {
			continue
		}
		if len(p.ApplyConditions) > 0 && !conditionsMatch(p.ApplyConditions, source, stream, true) {
			continue
		}
		if !conditionsMatch(p.Conditions, source, stream, false) {
			return false
		}
	}
	return true
}

func conditionsMatch(conditions []Condition, source MediaSource, stream Stream, applying bool) bool {
	for _, c := range conditions {
		value, known := property(source, stream, c.Property)
		if !known {
			if c.IsRequired || applying {
				return false
			}
			continue
		}
		equal := strings.EqualFold(value, c.Value)
		switch c.Condition {
		case "Equals":
			if !equal {
				return false
			}
		case "NotEquals":
			if equal {
				return false
			}
		case "EqualsAny":
			if !includes(strings.ReplaceAll(c.Value, "|", ","), value) {
				return false
			}
		case "LessThanEqual", "GreaterThanEqual":
			x, e1 := strconv.ParseFloat(value, 64)
			y, e2 := strconv.ParseFloat(c.Value, 64)
			if e1 != nil || e2 != nil || c.Condition == "LessThanEqual" && x > y || c.Condition == "GreaterThanEqual" && x < y {
				return false
			}
		default:
			if c.IsRequired || applying {
				return false
			}
		}
	}
	return true
}

func property(source MediaSource, s Stream, name string) (string, bool) {
	var n float64
	switch name {
	case "AudioCodec", "VideoCodec":
		return s.Codec, s.Codec != ""
	case "AudioProfile", "VideoProfile":
		return s.Profile, s.Profile != ""
	case "VideoRange":
		return s.VideoRange, s.VideoRange != ""
	case "VideoCodecTag":
		return s.CodecTag, s.CodecTag != ""
	case "IsInterlaced":
		return strconv.FormatBool(s.IsInterlaced), true
	case "IsAnamorphic":
		return strconv.FormatBool(s.IsAnamorphic), true
	case "IsSecondaryAudio":
		return strconv.FormatBool(source.DefaultAudioStreamIndex != nil && s.Index != *source.DefaultAudioStreamIndex), true
	case "IsExternalAudio":
		return strconv.FormatBool(s.IsExternal), true
	case "AudioChannels":
		n = float64(s.Channels)
	case "AudioBitrate", "VideoBitrate":
		n = float64(s.BitRate)
	case "AudioSampleRate":
		n = float64(s.SampleRate)
	case "AudioBitDepth", "VideoBitDepth":
		n = float64(s.BitDepth)
	case "Width":
		n = float64(s.Width)
	case "Height":
		n = float64(s.Height)
	case "VideoLevel":
		n = s.Level
	case "RefFrames":
		n = float64(s.RefFrames)
	case "VideoFramerate":
		n = s.RealFrameRate
		if n <= 0 {
			n = s.AverageFrameRate
		}
	case "NumVideoStreams", "NumAudioStreams":
		kind := "Video"
		if name == "NumAudioStreams" {
			kind = "Audio"
		}
		for _, v := range source.MediaStreams {
			if v.Type == kind {
				n++
			}
		}
	default:
		return "", false
	}
	return strconv.FormatFloat(n, 'f', -1, 64), n > 0
}
