package session

import (
	"errors"
	"fmt"
)

// ErrAuthRejected marks an error as a confirmed server-side authentication rejection
// (a real ec=/errorCode= response), as opposed to a network/dial/local-I/O failure that
// merely prevented the login attempt from completing. main.go uses this to choose exit
// code 2 (session is stale, needs a fresh capture) vs exit code 1 (generic failure) --
// see README's cron section for the documented contract this exists to satisfy.
var ErrAuthRejected = errors.New("server rejected authentication")

// ErrTokenRejected is the E011 case of ErrAuthRejected (it wraps it, so errors.Is still matches
// ErrAuthRejected): the server no longer accepts the access token. Seen live as the whole failure
// signature of a rotated token (2026-08-16, 2026-10-02) -- the client maps E011 to
// ClearAccessToken() -- and the only fix is a fresh capture of the real app logging in.
var ErrTokenRejected = fmt.Errorf("access token rejected (E011): %w", ErrAuthRejected)
