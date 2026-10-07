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

// ErrSessionEnded marks a run the server itself ended with a terminal push. ErrKicked,
// ErrServerStopped and ErrInitFailed all wrap it, so one errors.Is check covers the three. The
// real client does not reconnect after any of them, so neither may a cron run: main.go exits
// with code 3 instead of 1. Static only: none of these pushes has been seen live yet.
var ErrSessionEnded = errors.New("server ended the session")

var (
	// ErrKicked is push.user.off: the account logged in elsewhere. The real client sets
	// pushOffWithQuitGame, shows the errorCode text (E100083 when the push has none) and quits;
	// IsNeedReConnect then refuses every reconnect (Assembly-CSharp.decompiled.cs:83447-83472,
	// 8251-8271).
	ErrKicked = fmt.Errorf("%w: kicked, the account logged in elsewhere (push.user.off, client text E100083)", ErrSessionEnded)

	// ErrServerStopped is push.server.stop: the server is going down for maintenance. The real
	// client closes chat and opens UIExitGameTip ("Server disconnected. Please restart the
	// game."), whose only button quits (Net/Msgs/PushServerStopMessage.lua:6-10). The earlier
	// push.server.tostop is only a countdown toast and is not terminal.
	ErrServerStopped = fmt.Errorf("%w: server stopping for maintenance (push.server.stop)", ErrSessionEnded)

	// ErrInitFailed is init.error: the server could not build the session's init. The real client
	// fails the load with E106 (Loading.OnInitError, Assembly-CSharp.decompiled.cs:82111-82126,
	// 7419-7423). Without this, Go would sit out the whole 45 s init wait.
	ErrInitFailed = fmt.Errorf("%w: server failed to initialise the session (init.error, client load error E106)", ErrSessionEnded)
)

// terminalPushes maps each terminal push's cmd to its sentinel.
var terminalPushes = map[string]error{
	"push.user.off":    ErrKicked,
	"push.server.stop": ErrServerStopped,
	"init.error":       ErrInitFailed,
}

// SessionEndedError is what ReadEnvelope returns for a terminal push, and on every later read or
// send on the same GameConn. It is a net.Error with Timeout()==false, like sfs.DeadConnError, so
// every existing "abort on a dead connection" check (ContainsNonTimeoutNetError, the
// errors.As(&netErr) && !netErr.Timeout() pattern) stops the run without knowing about it.
// errors.Is(err, ErrKicked) and friends match through Unwrap.
type SessionEndedError struct {
	Cmd  string // the push: push.user.off, push.server.stop or init.error
	Code string // the push's errorCode field, if it carried one
	Err  error  // ErrKicked, ErrServerStopped or ErrInitFailed
}

func (e *SessionEndedError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%v (errorCode=%s)", e.Err, e.Code)
	}
	return e.Err.Error()
}
func (e *SessionEndedError) Unwrap() error { return e.Err }
func (*SessionEndedError) Timeout() bool   { return false }
func (*SessionEndedError) Temporary() bool { return false }

// TerminalPushError returns the SessionEndedError for cmd if cmd is a terminal server push, or
// nil otherwise. ReadEnvelope already applies it to every frame it decodes, so read loops built
// on ReadEnvelope get the error without calling this; it exists for code that classifies an
// already-decoded ExtensionMessage itself.
func TerminalPushError(cmd string) error {
	sentinel, ok := terminalPushes[cmd]
	if !ok {
		return nil
	}
	return &SessionEndedError{Cmd: cmd, Err: sentinel}
}
