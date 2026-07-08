package gateway

import "time"

const (
	defaultSessionTTL = 30 * 24 * time.Hour
	gatewayVersion    = "0.0.0"

	defaultBackendUserAgent            = "SenPlayer/6.1.3"
	defaultBackendAuthorizationClient  = "SenPlayer"
	defaultBackendAuthorizationDevice  = "Mac"
	defaultBackendAuthorizationVersion = "6.1.3"
)
