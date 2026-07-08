package gateway

import "errors"

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrNotFound           = errors.New("not found")
	ErrDisabled           = errors.New("disabled")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrBadRequest         = errors.New("bad request")
)
