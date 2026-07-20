package gateway

import "encoding/json"

type nonNilStrings []string

func (values nonNilStrings) MarshalJSON() ([]byte, error) {
	if values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(values))
}

type nonNilRawMessages []json.RawMessage

func (values nonNilRawMessages) MarshalJSON() ([]byte, error) {
	if values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]json.RawMessage(values))
}

type embyAuthenticationResult struct {
	AccessToken string          `json:"AccessToken"`
	ServerID    string          `json:"ServerId"`
	User        embyUser        `json:"User"`
	SessionInfo embySessionInfo `json:"SessionInfo"`
}

type embyUser struct {
	Name                  string                 `json:"Name"`
	ServerID              string                 `json:"ServerId"`
	ServerName            string                 `json:"ServerName"`
	ID                    string                 `json:"Id"`
	HasPassword           bool                   `json:"HasPassword"`
	HasConfiguredPassword bool                   `json:"HasConfiguredPassword"`
	EnableAutoLogin       bool                   `json:"EnableAutoLogin"`
	Configuration         *embyUserConfiguration `json:"Configuration,omitempty"`
	Policy                *embyUserPolicy        `json:"Policy,omitempty"`
}

type embyUserConfiguration struct {
	PlayDefaultAudioTrack      bool          `json:"PlayDefaultAudioTrack"`
	SubtitleMode               string        `json:"SubtitleMode"`
	RememberAudioSelections    bool          `json:"RememberAudioSelections"`
	RememberSubtitleSelections bool          `json:"RememberSubtitleSelections"`
	EnableNextEpisodeAutoPlay  bool          `json:"EnableNextEpisodeAutoPlay"`
	HidePlayedInLatest         bool          `json:"HidePlayedInLatest"`
	HidePlayedInMoreLikeThis   bool          `json:"HidePlayedInMoreLikeThis"`
	HidePlayedInSuggestions    bool          `json:"HidePlayedInSuggestions"`
	EnableLocalPassword        bool          `json:"EnableLocalPassword"`
	DisplayMissingEpisodes     bool          `json:"DisplayMissingEpisodes"`
	ResumeRewindSeconds        int           `json:"ResumeRewindSeconds"`
	OrderedViews               nonNilStrings `json:"OrderedViews"`
	LatestItemsExcludes        nonNilStrings `json:"LatestItemsExcludes"`
	MyMediaExcludes            nonNilStrings `json:"MyMediaExcludes"`
}

type embyUserPolicy struct {
	IsAdministrator                  bool          `json:"IsAdministrator"`
	IsHidden                         bool          `json:"IsHidden"`
	IsDisabled                       bool          `json:"IsDisabled"`
	EnableUserPreferenceAccess       bool          `json:"EnableUserPreferenceAccess"`
	EnableRemoteControlOfOtherUsers  bool          `json:"EnableRemoteControlOfOtherUsers"`
	EnableSharedDeviceControl        bool          `json:"EnableSharedDeviceControl"`
	EnableRemoteAccess               bool          `json:"EnableRemoteAccess"`
	EnableMediaPlayback              bool          `json:"EnableMediaPlayback"`
	EnableAudioPlaybackTranscoding   bool          `json:"EnableAudioPlaybackTranscoding"`
	EnableVideoPlaybackTranscoding   bool          `json:"EnableVideoPlaybackTranscoding"`
	EnablePlaybackRemuxing           bool          `json:"EnablePlaybackRemuxing"`
	EnableContentDownloading         bool          `json:"EnableContentDownloading"`
	EnableLiveTVAccess               bool          `json:"EnableLiveTvAccess"`
	EnableLiveTVManagement           bool          `json:"EnableLiveTvManagement"`
	EnableUserCreatedContent         bool          `json:"EnableUserCreatedContent"`
	EnableCollectionManagement       bool          `json:"EnableCollectionManagement"`
	EnableSubtitleManagement         bool          `json:"EnableSubtitleManagement"`
	EnableContentDeletion            bool          `json:"EnableContentDeletion"`
	EnablePublicSharing              bool          `json:"EnablePublicSharing"`
	EnableContentDeletionFromFolders nonNilStrings `json:"EnableContentDeletionFromFolders"`
	RestrictedFeatures               nonNilStrings `json:"RestrictedFeatures"`
	EnableMediaConversion            bool          `json:"EnableMediaConversion"`
	EnableAllChannels                bool          `json:"EnableAllChannels"`
	EnableAllFolders                 bool          `json:"EnableAllFolders"`
	EnableAllDevices                 bool          `json:"EnableAllDevices"`
	BlockedTags                      nonNilStrings `json:"BlockedTags"`
	AccessSchedules                  nonNilStrings `json:"AccessSchedules"`
	BlockUnratedItems                nonNilStrings `json:"BlockUnratedItems"`
	EnabledChannels                  nonNilStrings `json:"EnabledChannels"`
	EnabledFolders                   nonNilStrings `json:"EnabledFolders"`
	EnabledDevices                   nonNilStrings `json:"EnabledDevices"`
}

type embySessionInfo struct {
	ID                    string            `json:"Id"`
	ServerID              string            `json:"ServerId"`
	UserID                string            `json:"UserId"`
	UserName              string            `json:"UserName"`
	Client                string            `json:"Client"`
	DeviceName            string            `json:"DeviceName"`
	DeviceID              string            `json:"DeviceId"`
	ApplicationVersion    string            `json:"ApplicationVersion"`
	SupportedCommands     nonNilStrings     `json:"SupportedCommands"`
	PlayableMediaTypes    nonNilStrings     `json:"PlayableMediaTypes"`
	AdditionalUsers       nonNilRawMessages `json:"AdditionalUsers"`
	SupportsRemoteControl bool              `json:"SupportsRemoteControl"`
	LastActivityDate      string            `json:"LastActivityDate"`
	PlayState             embyPlayState     `json:"PlayState"`
	NowPlayingItem        json.RawMessage   `json:"NowPlayingItem,omitempty"`
}

type embyPlayState struct {
	PositionTicks       *int64   `json:"PositionTicks,omitempty"`
	CanSeek             bool     `json:"CanSeek"`
	IsPaused            bool     `json:"IsPaused"`
	IsMuted             bool     `json:"IsMuted"`
	VolumeLevel         *int     `json:"VolumeLevel,omitempty"`
	AudioStreamIndex    *int     `json:"AudioStreamIndex,omitempty"`
	SubtitleStreamIndex *int     `json:"SubtitleStreamIndex,omitempty"`
	MediaSourceID       *string  `json:"MediaSourceId,omitempty"`
	PlayMethod          *string  `json:"PlayMethod,omitempty"`
	PlaybackRate        *float64 `json:"PlaybackRate,omitempty"`
	RepeatMode          *string  `json:"RepeatMode,omitempty"`
	Shuffle             *bool    `json:"Shuffle,omitempty"`
	SubtitleOffset      *float64 `json:"SubtitleOffset,omitempty"`
}

type embyClientCapabilities struct {
	PlayableMediaTypes   nonNilStrings   `json:"PlayableMediaTypes"`
	SupportedCommands    nonNilStrings   `json:"SupportedCommands"`
	SupportsMediaControl bool            `json:"SupportsMediaControl"`
	SupportsSync         bool            `json:"SupportsSync"`
	DeviceProfile        json.RawMessage `json:"DeviceProfile,omitempty"`
}

type embyUserItemData struct {
	Rating                *float64        `json:"Rating,omitempty"`
	PlaybackPositionTicks *int64          `json:"PlaybackPositionTicks,omitempty"`
	PlayCount             *int            `json:"PlayCount,omitempty"`
	IsFavorite            *bool           `json:"IsFavorite,omitempty"`
	Played                *bool           `json:"Played,omitempty"`
	PlayedPercentage      *float64        `json:"PlayedPercentage,omitempty"`
	LastPlayedDate        *string         `json:"LastPlayedDate,omitempty"`
	UnplayedItemCount     *int            `json:"UnplayedItemCount,omitempty"`
	Likes                 json.RawMessage `json:"Likes,omitempty"`
	Key                   string          `json:"Key,omitempty"`
	ItemID                string          `json:"ItemId,omitempty"`
}

type embyLocalQueryResult struct {
	Items            nonNilRawMessages `json:"Items"`
	TotalRecordCount int64             `json:"TotalRecordCount"`
}

type embySystemInfo struct {
	ID              string        `json:"Id"`
	ServerID        string        `json:"ServerId"`
	ServerName      string        `json:"ServerName"`
	Version         string        `json:"Version"`
	LocalAddress    string        `json:"LocalAddress"`
	WanAddress      string        `json:"WanAddress"`
	RemoteAddresses nonNilStrings `json:"RemoteAddresses"`
	LocalAddresses  nonNilStrings `json:"LocalAddresses"`
}

type registrationValidateDevice struct {
	CacheExpirationDays int    `json:"cacheExpirationDays"`
	Message             string `json:"message"`
	ResultCode          string `json:"resultCode"`
}

type registrationValidate struct {
	FeatureID  string `json:"featId"`
	Registered bool   `json:"registered"`
	Expires    string `json:"expDate"`
	Key        string `json:"key"`
}

type registrationStatus struct {
	DeviceStatus  int               `json:"deviceStatus"`
	PlanType      string            `json:"planType"`
	Subscriptions nonNilRawMessages `json:"subscriptions"`
}
