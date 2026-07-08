package gateway

import "time"

const (
	defaultSessionTTL = 30 * 24 * time.Hour
	gatewayVersion    = "0.3.0"

	backendAuthTimeout         = 15 * time.Second
	proxyResponseHeaderTimeout = 30 * time.Second
	proxyIdleConnTimeout       = 90 * time.Second
	loginFailureLimit          = 5
	loginFailureBlockDuration  = time.Minute
	proxyJSONLimit             = 20 << 20
	proxyM3U8Limit             = 20 << 20
	personalIDBatchLimit       = 200

	defaultBackendUserAgent            = "SenPlayer/6.1.3"
	defaultBackendAuthorizationClient  = "SenPlayer"
	defaultBackendAuthorizationDevice  = "Mac"
	defaultBackendAuthorizationVersion = "6.1.3"
)
