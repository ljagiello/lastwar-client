package app

import "errors"

// ErrAuthRejected marks an error as a confirmed server-side authentication rejection
// (a real ec=/errorCode= response), as opposed to a network/dial/local-I/O failure that
// merely prevented the login attempt from completing. main.go uses this to choose exit
// code 2 (session is stale, needs a fresh capture) vs exit code 1 (generic failure) --
// see README's cron section for the documented contract this exists to satisfy.
var ErrAuthRejected = errors.New("server rejected authentication")
