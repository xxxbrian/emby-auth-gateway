package gateway

import "errors"

var (
	ErrInvalidCredentials      = errors.New("invalid credentials")
	ErrNotFound                = errors.New("not found")
	ErrDisabled                = errors.New("disabled")
	ErrUnauthorized            = errors.New("unauthorized")
	ErrBadRequest              = errors.New("bad request")
	ErrUpstreamNotFound        = errors.New("upstream not found")
	ErrInvalidUpstreamTopology = errors.New("invalid upstream topology")
	ErrUpstreamAuthConflict    = errors.New("upstream authentication conflict")
	ErrStoreUnavailable        = errors.New("store unavailable")
)
