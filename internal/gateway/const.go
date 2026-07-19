package gateway

import "time"

const (
	defaultSessionTTL = 30 * 24 * time.Hour

	backendAuthTimeout              = 15 * time.Second
	backendLoginFailureCooldown     = 30 * time.Second
	backendTokenRefreshMinInterval  = 2 * time.Minute
	proxyResponseHeaderTimeout      = 30 * time.Second
	proxyIdleConnTimeout            = 90 * time.Second
	loginFailureLimit               = 5
	loginFailureBlockDuration       = time.Minute
	proxyJSONLimit                  = 20 << 20
	proxyM3U8Limit                  = 20 << 20
	personalIDBatchLimit            = 200
	aggregateChildCountLookups      = 5
	defaultMinResumePct             = 5.0
	defaultMaxResumePct             = 90.0
	defaultMinResumeDurationSeconds = 300.0
	embyTicksPerSecond              = int64(10_000_000)

	defaultBackendUserAgent            = "SenPlayer/6.1.3"
	defaultBackendAuthorizationClient  = "SenPlayer"
	defaultBackendAuthorizationDevice  = "Mac"
	defaultBackendAuthorizationVersion = "6.1.3"
	defaultBackendServerVersion        = "4.8.11.0"
)
