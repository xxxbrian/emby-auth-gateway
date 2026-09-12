// Package transcode plans and serves bounded audio conversion for seekable media.
// It owns conversion work, not gateway authentication or playback UserData.
package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const TicksPerSecond int64 = 10_000_000

var (
	ErrUnsupported = errors.New("conversion requires unsupported media capabilities")
	ErrIndex       = errors.New("a reliable media seek index is unavailable")
	ErrSource      = errors.New("media source is unavailable or changed")
	ErrCapacity    = errors.New("conversion capacity is exhausted")
	ErrNotFound    = errors.New("conversion playback is unavailable")
	ErrWorker      = errors.New("audio conversion failed")
)

type Stream struct {
	Index            int     `json:"Index"`
	Type             string  `json:"Type"`
	Codec            string  `json:"Codec"`
	Profile          string  `json:"Profile"`
	Level            float64 `json:"Level"`
	Channels         int     `json:"Channels"`
	ChannelLayout    string  `json:"ChannelLayout"`
	BitRate          int64   `json:"BitRate"`
	SampleRate       int     `json:"SampleRate"`
	BitDepth         int     `json:"BitDepth"`
	Width            int     `json:"Width"`
	Height           int     `json:"Height"`
	RefFrames        int     `json:"RefFrames"`
	RealFrameRate    float64 `json:"RealFrameRate"`
	AverageFrameRate float64 `json:"AverageFrameRate"`
	VideoRange       string  `json:"VideoRange"`
	CodecTag         string  `json:"CodecTag"`
	IsInterlaced     bool    `json:"IsInterlaced"`
	IsAnamorphic     bool    `json:"IsAnamorphic"`
	IsExternal       bool    `json:"IsExternal"`
	IsDefault        bool    `json:"IsDefault"`
	DeliveryMethod   string  `json:"DeliveryMethod"`
	DeliveryURL      string  `json:"DeliveryUrl"`
}

type MediaSource struct {
	ID                         string            `json:"Id"`
	Name                       string            `json:"Name"`
	Container                  string            `json:"Container"`
	Size                       int64             `json:"Size"`
	Bitrate                    int64             `json:"Bitrate"`
	RunTimeTicks               int64             `json:"RunTimeTicks"`
	MediaStreams               []Stream          `json:"MediaStreams"`
	DefaultAudioStreamIndex    *int              `json:"DefaultAudioStreamIndex"`
	DefaultSubtitleStreamIndex *int              `json:"DefaultSubtitleStreamIndex"`
	DirectStreamURL            string            `json:"DirectStreamUrl"`
	TranscodingURL             string            `json:"TranscodingUrl"`
	SupportsTranscoding        bool              `json:"SupportsTranscoding"`
	RequiredHTTPHeaders        map[string]string `json:"RequiredHttpHeaders"`
}

// Source opens exactly [offset, offset+length). Calls must remain cancellable.
// The gateway adapter owns credentials and verifies the source version.
type Source struct {
	Media MediaSource
	Open  func(context.Context, int64, int64) (io.ReadCloser, error)
	Valid func(context.Context) bool
}

type Profile struct {
	DirectPlayProfiles  []FormatProfile    `json:"DirectPlayProfiles"`
	TranscodingProfiles []FormatProfile    `json:"TranscodingProfiles"`
	CodecProfiles       []CodecProfile     `json:"CodecProfiles"`
	ContainerProfiles   []ContainerProfile `json:"ContainerProfiles"`
	MaxStreamingBitrate int64              `json:"MaxStreamingBitrate"`
}

type FormatProfile struct {
	Type             string `json:"Type"`
	Container        string `json:"Container"`
	VideoCodec       string `json:"VideoCodec"`
	AudioCodec       string `json:"AudioCodec"`
	Context          string `json:"Context"`
	Protocol         string `json:"Protocol"`
	MaxAudioChannels string `json:"MaxAudioChannels"`
}

type Condition struct {
	Condition  string `json:"Condition"`
	Property   string `json:"Property"`
	Value      string `json:"Value"`
	IsRequired bool   `json:"IsRequired"`
}

// Emby Web emits IsRequired as both a JSON boolean and a boolean string.
// Normalize the wire representation once, before evaluating any capabilities.
func (c *Condition) UnmarshalJSON(data []byte) error {
	var wire struct {
		Condition  string
		Property   string
		Value      string
		IsRequired json.RawMessage
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.Condition, c.Property, c.Value = wire.Condition, wire.Property, wire.Value
	c.IsRequired = false
	if len(wire.IsRequired) == 0 || string(wire.IsRequired) == "null" {
		return nil
	}
	value, err := strconv.ParseBool(strings.Trim(string(wire.IsRequired), "\""))
	if err != nil {
		return err
	}
	c.IsRequired = value
	return nil
}

type CodecProfile struct {
	Type            string      `json:"Type"`
	Codec           string      `json:"Codec"`
	Container       string      `json:"Container"`
	Conditions      []Condition `json:"Conditions"`
	ApplyConditions []Condition `json:"ApplyConditions"`
}

type ContainerProfile struct {
	Type       string      `json:"Type"`
	Container  string      `json:"Container"`
	Conditions []Condition `json:"Conditions"`
}

type Request struct {
	Profile              Profile
	AudioStreamIndex     *int
	SubtitleStreamIndex  *int
	MaxAudioChannels     int
	MaxStreamingBitrate  int64
	EnableDirectPlay     *bool
	EnableDirectStream   *bool
	EnableTranscoding    *bool
	AllowAudioStreamCopy *bool
	AllowVideoStreamCopy *bool
}

type Plan struct {
	Container     string
	Video         Stream
	Audio         Stream
	AudioCodec    string
	AudioChannels int
	AudioBitrate  int64
	Reason        string
}

type Config struct {
	BootID      string
	FFmpegPath  string
	CacheDir    string
	CacheBytes  int64
	Workers     int
	MaxJobs     int
	IdleTTL     time.Duration
	WorkTimeout time.Duration
}

type Identity struct {
	SourceRef string // credential-free source captured when the job is created
	Owner     string // login token hash; never exposed through observation
	UserID    string
	Username  string
	Device    string
	ItemID    string
	ItemName  string
}
