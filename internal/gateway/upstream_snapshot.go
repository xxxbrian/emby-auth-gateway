package gateway

// upstreamRequestSnapshot is an immutable projection of the legacy session
// fields consumed by one upstream request attempt.
type upstreamRequestSnapshot struct {
	baseURL  string
	serverID string
	userID   string
	token    string
	identity BackendClientIdentity
}

func upstreamRequestSnapshotFromLegacySession(session *Session) upstreamRequestSnapshot {
	if session == nil {
		return upstreamRequestSnapshot{}
	}
	return upstreamRequestSnapshot{
		baseURL: session.BackendBaseURL, serverID: session.BackendServerID,
		userID: session.BackendUserID, token: session.BackendToken,
		identity: session.BackendIdentity,
	}
}
