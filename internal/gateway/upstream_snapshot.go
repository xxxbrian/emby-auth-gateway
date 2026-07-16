package gateway

// upstreamRequestSnapshot is an immutable projection of one authoritative
// singleton upstream runtime, consumed by one request attempt.
type upstreamRequestSnapshot struct {
	baseURL  string
	serverID string
	userID   string
	token    string
	identity BackendClientIdentity
}

func upstreamRequestSnapshotFromRuntime(runtime *UpstreamRuntime) (upstreamRequestSnapshot, error) {
	if runtime == nil {
		return upstreamRequestSnapshot{}, invalidUpstreamTopology("missing runtime")
	}
	if err := ValidateUpstreamRuntime(*runtime); err != nil {
		return upstreamRequestSnapshot{}, err
	}
	if err := validatePersistedUpstreamAuth(runtime.Source); err != nil {
		return upstreamRequestSnapshot{}, err
	}
	return upstreamRequestSnapshot{baseURL: runtime.Endpoint.BaseURL, serverID: runtime.Source.ServerID, userID: runtime.Source.BackendUserID, token: runtime.Source.BackendToken, identity: runtime.Source.ClientIdentity}, nil
}
