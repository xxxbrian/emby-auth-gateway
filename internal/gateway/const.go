package gateway

import "time"

const (
	defaultSessionTTL = 30 * 24 * time.Hour
	gatewayVersion    = "0.0.0"

	defaultBackendUserAgent             = "SenPlayer/6.1.3"
	defaultBackendAuthorizationClient   = "SenPlayer"
	defaultBackendAuthorizationDevice   = "Mac"
	defaultBackendAuthorizationDeviceID = "E680121A-04F6-4E47-BA8F-30E1DB01EFB6"
	defaultBackendAuthorizationVersion  = "6.1.3"
)
