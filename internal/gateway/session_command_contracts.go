package gateway

type SessionCommandCategory uint8

const (
	SessionCommandGeneral SessionCommandCategory = iota + 1
	SessionCommandPlay
	SessionCommandPlaystate
)

type GeneralCommand struct {
	Name         string
	Text         string
	Volume       *int
	Index        *int
	ItemType     string
	ItemID       string
	ItemName     string
	Header       string
	TimeoutMS    *int64
	PlaybackRate *float64
}

type PlayCommand struct {
	Command             string
	ItemIDs             []string
	StartPositionTicks  *int64
	MediaSourceID       string
	AudioStreamIndex    *int
	SubtitleStreamIndex *int
	StartIndex          *int
}

type PlaystateCommand struct {
	Name              string
	SeekPositionTicks *int64
}

// SessionCommandEnvelope is a closed typed union for local command delivery.
type SessionCommandEnvelope struct {
	Category  SessionCommandCategory
	General   *GeneralCommand
	Play      *PlayCommand
	Playstate *PlaystateCommand
}
