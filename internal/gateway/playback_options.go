package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

const playbackOptionsBodyLimit = 2 << 20

var errPlaybackOption = errors.New("unsupported or conflicting playback option")

type playbackOptionsKey struct{}

type playbackOptions struct {
	previousID string
	request    transcode.Request
	sourceID   string
	start      int64
	playback   bool
}

type playbackParameters struct {
	query url.Values
	body  map[string][]json.RawMessage
}

func withPlaybackOptions(r *http.Request) *http.Request {
	var data []byte
	if r.Method == http.MethodPost && r.Body != nil && r.Body != http.NoBody {
		original := r.Body
		var err error
		data, err = io.ReadAll(io.LimitReader(original, playbackOptionsBodyLimit+1))
		var replay io.Reader = bytes.NewReader(data)
		switch {
		case err != nil:
			replay = &replayReadErrorReader{prefix: data, err: err, errorPending: true, remainder: original}
		case len(data) > playbackOptionsBodyLimit:
			replay = io.MultiReader(replay, original)
		}
		r.Body = &playbackReplayBody{Reader: replay, Closer: original}
		if err != nil || len(data) > playbackOptionsBodyLimit {
			return r
		}
	}
	options, err := parsePlaybackOptions(r.Method, r.URL.Query(), data)
	if err != nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), playbackOptionsKey{}, options))
}

type playbackReplayBody struct {
	io.Reader
	io.Closer
}

func parsePlaybackOptions(method string, query url.Values, data []byte) (*playbackOptions, error) {
	body, err := playbackFields(data)
	if err != nil {
		return nil, err
	}
	params := playbackParameters{query: query, body: body}
	var options playbackOptions
	profiles := body["deviceprofile"]
	if len(profiles) > 1 {
		return nil, errPlaybackOption
	}
	if len(profiles) == 1 {
		if err := json.Unmarshal(profiles[0], &options.request.Profile); err != nil {
			return nil, err
		}
	}
	var playback *bool
	for _, field := range []struct {
		name string
		dest **bool
	}{
		{"EnableDirectPlay", &options.request.EnableDirectPlay},
		{"EnableDirectStream", &options.request.EnableDirectStream},
		{"EnableTranscoding", &options.request.EnableTranscoding},
		{"AllowAudioStreamCopy", &options.request.AllowAudioStreamCopy},
		{"AllowVideoStreamCopy", &options.request.AllowVideoStreamCopy},
		{"IsPlayback", &playback},
	} {
		*field.dest, err = playbackOption(params, field.name, true, parsePlaybackBool)
		if err != nil {
			return nil, err
		}
	}
	options.playback = method == http.MethodPost && playback != nil && *playback
	if options.request.AudioStreamIndex, err = playbackStreamIndex(params, "AudioStreamIndex", 0); err != nil {
		return nil, err
	}
	if options.request.SubtitleStreamIndex, err = playbackStreamIndex(params, "SubtitleStreamIndex", -1); err != nil {
		return nil, err
	}
	channels, err := playbackInteger(params, "MaxAudioChannels", 0, 32)
	if err != nil {
		return nil, err
	}
	if channels != nil {
		options.request.MaxAudioChannels = int(*channels)
	}
	for _, field := range []struct {
		name string
		dest *int64
	}{{"MaxStreamingBitrate", &options.request.MaxStreamingBitrate}, {"StartTimeTicks", &options.start}} {
		value, err := playbackInteger(params, field.name, 0, 1<<63-1)
		if err != nil {
			return nil, err
		}
		if value != nil {
			*field.dest = *value
		}
	}
	if options.sourceID, err = playbackID(params, "MediaSourceId"); err != nil {
		return nil, err
	}
	if options.previousID, err = playbackID(params, "CurrentPlaySessionId"); err != nil {
		return nil, err
	}
	return &options, nil
}

func playbackFields(data []byte) (map[string][]json.RawMessage, error) {
	data = bytes.Trim(data, " \r\n\t")
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errPlaybackOption
	}
	fields := make(map[string][]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		name := strings.ToLower(token.(string))
		fields[name] = append(fields[name], raw)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.InputOffset() != int64(len(data)) {
		return nil, errPlaybackOption
	}
	return fields, nil
}

func playbackOption[T comparable](params playbackParameters, name string, allowLiteral bool, parse func(string) (T, error)) (*T, error) {
	var result *T
	add := func(text string) error {
		value, err := parse(text)
		if err != nil {
			return err
		}
		if result != nil && *result != value {
			return errPlaybackOption
		}
		result = &value
		return nil
	}
	for key, values := range params.query {
		if strings.EqualFold(key, name) {
			for _, text := range values {
				if err := add(text); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, raw := range params.body[strings.ToLower(name)] {
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		text := string(raw)
		if raw[0] == '"' || !allowLiteral {
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, err
			}
		}
		if err := add(text); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func parsePlaybackBool(text string) (bool, error) {
	return strconv.ParseBool(strings.TrimSpace(text))
}

func playbackInteger(params playbackParameters, name string, minimum, maximum int64) (*int64, error) {
	return playbackOption(params, name, true, func(text string) (int64, error) {
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || value < minimum || value > maximum {
			return 0, errPlaybackOption
		}
		return value, nil
	})
}

func playbackStreamIndex(params playbackParameters, name string, minimum int64) (*int, error) {
	value, err := playbackInteger(params, name, minimum, 1024)
	if err != nil || value == nil {
		return nil, err
	}
	index := int(*value)
	return &index, nil
}

func playbackID(params playbackParameters, name string) (string, error) {
	value, err := playbackOption(params, name, false, func(text string) (string, error) {
		return text, nil
	})
	if err != nil || value == nil {
		return "", err
	}
	return *value, nil
}
