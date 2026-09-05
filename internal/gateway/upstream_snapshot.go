package gateway

import "fmt"

// upstreamRequestSnapshot is an immutable projection of one authoritative
// singleton upstream runtime, consumed by one request attempt. EndpointKey
// records which endpoint the request was routed to ("" means the default).
type upstreamRequestSnapshot struct {
	baseURL     string
	endpointKey string
	serverID    string
	userID      string
	token       string
	identity    BackendClientIdentity
}

// upstreamRequestSnapshotFromRuntime projects the runtime with the default
// (fallback) endpoint. It is used for ordinary requests and by callers that
// operate on the runtime before per-request endpoint selection.
func upstreamRequestSnapshotFromRuntime(runtime *UpstreamRuntime) (upstreamRequestSnapshot, error) {
	return upstreamRequestSnapshotFromRuntimeEndpoint(runtime, "")
}

// upstreamRequestSnapshotFromRuntimeEndpoint projects the runtime with the
// given endpoint key. An empty key selects the default endpoint.
func upstreamRequestSnapshotFromRuntimeEndpoint(runtime *UpstreamRuntime, endpointKey string) (upstreamRequestSnapshot, error) {
	if runtime == nil {
		return upstreamRequestSnapshot{}, invalidUpstreamTopology("missing runtime")
	}
	if err := ValidateUpstreamRuntime(*runtime); err != nil {
		return upstreamRequestSnapshot{}, err
	}
	if err := validatePersistedUpstreamAuth(runtime.Source); err != nil {
		return upstreamRequestSnapshot{}, err
	}
	var endpoint *UpstreamEndpoint
	if endpointKey == "" {
		endpoint, _ = runtime.DefaultEndpoint()
	} else {
		endpoint = runtime.Endpoints.EnabledByKey(endpointKey)
	}
	if endpoint == nil {
		return upstreamRequestSnapshot{}, invalidUpstreamTopology(fmt.Sprintf("endpoint %q unavailable", endpointKey))
	}
	return upstreamRequestSnapshot{baseURL: endpoint.BaseURL, endpointKey: endpoint.Key, serverID: runtime.Source.ServerID, userID: runtime.Source.BackendUserID, token: runtime.Source.BackendToken, identity: runtime.Source.ClientIdentity}, nil
}
