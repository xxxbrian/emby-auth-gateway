package gateway

func publicUserWireDTO(user GatewayUser, serverID string) embyUser {
	return embyUser{
		Name:                  user.Username,
		ServerID:              serverID,
		ServerName:            "Emby Gateway",
		ID:                    user.SyntheticUserID,
		HasPassword:           true,
		HasConfiguredPassword: true,
	}
}

func currentUserWireDTO(username, syntheticID, serverID string) embyUser {
	user := publicUserWireDTO(GatewayUser{Username: username, SyntheticUserID: syntheticID}, serverID)
	user.Configuration = &embyUserConfiguration{
		PlayDefaultAudioTrack: true, SubtitleMode: "Smart", RememberAudioSelections: true,
		RememberSubtitleSelections: true, EnableNextEpisodeAutoPlay: true, HidePlayedInLatest: true,
	}
	user.Policy = &embyUserPolicy{
		EnableUserPreferenceAccess: true, EnableRemoteAccess: true, EnableMediaPlayback: true,
		EnableAudioPlaybackTranscoding: true, EnableVideoPlaybackTranscoding: true, EnablePlaybackRemuxing: true,
		EnableContentDownloading: true, EnableAllChannels: true, EnableAllFolders: true, EnableAllDevices: true,
	}
	return user
}

func authenticationResultWireDTO(user GatewayUser, session *Session, token, serverID string) embyAuthenticationResult {
	return embyAuthenticationResult{
		AccessToken: token,
		ServerID:    serverID,
		User:        currentUserWireDTO(user.Username, user.SyntheticUserID, serverID),
		SessionInfo: sessionInfoWireDTO(session, serverID, nil, nil, false),
	}
}
