package session

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"lastwar-client/internal/sfs"
)

// ErrAuthRejected marks an error as a confirmed server-side authentication rejection
// (a real ec=/errorCode= response), as opposed to a network/dial/local-I/O failure that
// merely prevented the login attempt from completing. main.go uses this to choose exit
// code 2 (session is stale, needs a fresh capture) vs exit code 1 (generic failure), or 4 for its
// ErrAccountBlocked case -- see README's cron section for the documented contract this exists to
// satisfy.
var ErrAuthRejected = errors.New("server rejected authentication")

// ErrTokenRejected is the E011 case of ErrAuthRejected (it wraps it, so errors.Is still matches
// ErrAuthRejected): the server no longer accepts the access token. Seen live as the whole failure
// signature of a rotated token (2026-08-16, 2026-10-02) -- the client maps E011 to
// ClearAccessToken() -- and the only fix is a fresh capture of the real app logging in.
var ErrTokenRejected = fmt.Errorf("access token rejected (E011): %w", ErrAuthRejected)

// ErrAccountBlocked is the ErrAuthRejected case where the server refused the account, role,
// device or IP: a ban, an IP or device blacklist, traffic control, or an account or role deletion
// state. These are the numeric ep[0] codes of the client's LoginErrorCode table
// (Global/EnumType.lua:7-41); the real client never retries them. A fresh capture or a token
// refresh can't change them, so main.go exits with its own code instead of the "recapture" 2.
// Every Login rejection error built from one is an *AccountBlockedError.
var ErrAccountBlocked = fmt.Errorf("login blocked: %w", ErrAuthRejected)

// loginBanCode is ERROR_BANID, the only blocked code whose ep[0] details are read here.
const loginBanCode = "4"

// loginBanAllTime is the explicit all-time ban time (long.MaxValue - 1), per discussion #53.
const loginBanAllTime = "9223372036854775806"

// ErrLoginMaintenanceOrIdentity is E005, still an ErrAuthRejected (exit 2). The 1.0.370 client
// shows it as server maintenance, E109 (LoginState.OnLoginResponse,
// Assembly-CSharp.decompiled.cs:13627-13628), but every E005 this project has recorded was an
// identity or ta mismatch (docs/auth.mdx). The reply can't tell the two apart; which exit code it
// gets is the owner's decision.
var ErrLoginMaintenanceOrIdentity = fmt.Errorf("E005, which the client shows as server maintenance (E109); every E005 seen so far was an identity or ta mismatch: %w", ErrAuthRejected)

// ErrSeasonWindow is a Login refused while a season opens (E015, ERROR_SEASON_WILL_START) or
// closes (E016, ERROR_SEASON_STOP_MAINTENANCE): a server state, not a credential problem. The
// client offers a reload or quit and keeps every credential (Assembly-CSharp.decompiled.cs:
// 13474-13491, constants 220289-220291). It does not wrap ErrAuthRejected, so main.go exits 1,
// not the "recapture" 2, and the next scheduled run tries again.
var ErrSeasonWindow = errors.New("login refused while a season opens or closes; not a credential problem, the next run tries again")

// seasonWindowCodes are the ep[0] codes of ErrSeasonWindow.
var seasonWindowCodes = map[string]string{
	"E015": "season opening (E015)",
	"E016": "season closing for maintenance (E016)",
}

// blockedLoginCodes labels the numeric ep[0] codes (Global/EnumType.lua:7-41; 55-58 are new in
// 1.0.364). The client parses them only when ep[0] contains a ';' (Assembly-CSharp.decompiled.cs:
// 13655-13658): 4 and 49 carry details, 47 and 57 can (array[2], 13695-13700 and 13716-13721), and
// 43 and 44 have no case in LoginState at all. Go splits on ';' whether or not one is there, which
// accepts everything the client does.
var blockedLoginCodes = map[string]string{
	loginBanCode: "ban",
	"43":         "IP blacklist",
	"44":         "device blacklist",
	"45":         "account deletion applied",
	"46":         "account deleted",
	"47":         "account deletion refused",
	"48":         "account deletion confirmed",
	"49":         "traffic control ban",
	"55":         "role deletion applied",
	"56":         "role deleted",
	"57":         "role deletion refused",
	"58":         "role deletion confirmed",
}

// AccountBlockedError is a Login rejected with one of blockedLoginCodes. errors.Is matches
// ErrAccountBlocked and ErrAuthRejected through Unwrap.
//
// A ban's ep[0] is "4;banTime;banType;reason;roleName", or "4;banTime;banType;roleName" with no
// reason (UIAccountBanView.lua:155-176, as C485 reported in discussion #53; not re-checked against
// this repo's decompile). The client only looks at banTime's sign: negative is permanent. The
// sign is read from the text, so the number is never parsed.
type AccountBlockedError struct {
	Code      string // ep[0]'s first ;-field, e.g. "4"
	Label     string // blockedLoginCodes[Code]
	Permanent bool   // a ban whose banTime is negative or loginBanAllTime
	BanTime   string // a ban's banTime as sent; when not Permanent, the expiry the client displays
	Reason    string // a ban's reason code, e.g. "703"; empty in the four-field form
}

func (e *AccountBlockedError) Error() string {
	detail := e.Label
	if e.Code == loginBanCode {
		switch {
		case e.Permanent:
			detail += ", permanent"
		case e.BanTime != "":
			detail += ", until " + e.BanTime
		}
		if e.Reason != "" {
			detail += ", reason " + e.Reason
		}
	}
	return fmt.Sprintf("%s (ep code %s): %v", detail, e.Code, ErrAccountBlocked)
}
func (e *AccountBlockedError) Unwrap() error { return ErrAccountBlocked }

// LoginRejection returns the error a rejected Login (one whose response carries ec, SFS2X's
// custom-error code 28) wraps, read from ep the way LoginState.OnLoginResponse reads it: ep is a
// one-element string array, and a ban's details are ;-separated inside that one string.
// ErrTokenRejected for E011, ErrLoginMaintenanceOrIdentity for E005, ErrSeasonWindow (not an auth
// rejection) for E015/E016, an *AccountBlockedError for blockedLoginCodes, else ErrAuthRejected.
func LoginRejection(content *sfs.SFSObject) error {
	ep, _ := content.Get("ep")
	codes, _ := ep.Val.([]string)
	if slices.Contains(codes, "E011") {
		return ErrTokenRejected
	}
	if len(codes) == 0 {
		return ErrAuthRejected
	}
	if codes[0] == "E005" {
		return ErrLoginMaintenanceOrIdentity
	}
	if label, ok := seasonWindowCodes[codes[0]]; ok {
		return fmt.Errorf("%s: %w", label, ErrSeasonWindow)
	}
	fields := strings.Split(codes[0], ";")
	label, ok := blockedLoginCodes[fields[0]]
	if !ok {
		return ErrAuthRejected
	}
	blocked := &AccountBlockedError{Code: fields[0], Label: label}
	if blocked.Code == loginBanCode && len(fields) > 1 {
		blocked.BanTime = fields[1]
		blocked.Permanent = strings.HasPrefix(fields[1], "-") || fields[1] == loginBanAllTime
		if len(fields) >= 5 {
			blocked.Reason = fields[3]
		}
	}
	return blocked
}

// ErrSessionEnded marks a run the server itself ended with a terminal push. Every terminal-push
// sentinel below wraps it, so one errors.Is check covers them all. The real client does not
// reconnect after any of them, so neither may a cron run: main.go exits with code 3 instead of 1.
// push.user.off is seen live (it often ends the 02:00 UTC run); the others are static only.
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

	// ErrGMKicked is push.user.gm.off: a GM took the session. The client shows E130023 with two
	// buttons, quit and reload, and nothing else (Net/Msgs/PushUserGMOffMessage.lua:9-16).
	ErrGMKicked = fmt.Errorf("%w: removed by a GM (push.user.gm.off, client text E130023)", ErrSessionEnded)

	// ErrRefundRestart is push.refund.restart without an errorCode: after a refund the client's
	// only way on is a reload (Net/Msgs/PushRefundRestartMessage.lua:8-27). With an errorCode it
	// only shows a tip and is not terminal.
	ErrRefundRestart = fmt.Errorf("%w: restart required after a refund (push.refund.restart)", ErrSessionEnded)

	// ErrDeviceRemoved is push.account.device.del, without an errorCode, naming this session's
	// device: the device was removed from the account, and the client clears every stored
	// credential and reloads (Net/Msgs/Account/PushAccountDeviceDelMessage.lua:8-25). It also
	// wraps ErrAuthRejected, because the stored login is dead and only a new bind or capture
	// brings it back; main.go must check it before ErrSessionEnded to exit 2 rather than 3.
	ErrDeviceRemoved = fmt.Errorf("%w: this device was removed from the account (push.account.device.del), so its stored login no longer works: %w", ErrSessionEnded, ErrAuthRejected)
)

// The pushes that end the session only in some shapes (terminalPush).
const (
	pushRefundRestartCmd = "push.refund.restart"
	pushDeviceDelCmd     = "push.account.device.del"
)

// terminalPushes maps each push that always ends the session to its sentinel.
var terminalPushes = map[string]error{
	"push.user.off":    ErrKicked,
	"push.server.stop": ErrServerStopped,
	"init.error":       ErrInitFailed,
	"push.user.gm.off": ErrGMKicked,
}

// terminalPush returns the SessionEndedError for a push, or nil when it does not end the session.
// ownDeviceID is the session's Login deviceId ("" when unknown).
func terminalPush(cmd string, params *sfs.SFSObject, ownDeviceID string) *SessionEndedError {
	if sentinel, ok := terminalPushes[cmd]; ok {
		return &SessionEndedError{Cmd: cmd, Err: sentinel}
	}
	if ec, ok := params.Get("errorCode"); ok && ec.Val != nil {
		return nil // the refund and device pushes only show the code as a tip then
	}
	switch {
	case cmd == pushRefundRestartCmd:
		return &SessionEndedError{Cmd: cmd, Err: ErrRefundRestart}
	case cmd == pushDeviceDelCmd && namesDevice(params, ownDeviceID):
		return &SessionEndedError{Cmd: cmd, Err: ErrDeviceRemoved}
	}
	return nil
}

// namesDevice is the client's own-device test, `t.deviceId == DEVICE_ID or t.airKey ==
// SetDeviceUidToTranscoding(DEVICE_ID)` (PushAccountDeviceDelMessage.lua:15-17), where the
// transcoding is "lwDid_" + base64 of the id (Assembly-CSharp.decompiled.cs:73443-73447). An
// unknown own id, or an empty pushed one, never matches.
func namesDevice(params *sfs.SFSObject, ownDeviceID string) bool {
	if ownDeviceID == "" {
		return false
	}
	if id := params.GetString("deviceId"); id != "" && id == ownDeviceID {
		return true
	}
	airKey := params.GetString("airKey")
	return airKey != "" && airKey == "lwDid_"+base64.StdEncoding.EncodeToString([]byte(ownDeviceID))
}

// SessionEndedError is what ReadEnvelope returns for a terminal push, and on every later read or
// send on the same GameConn. It is a net.Error with Timeout()==false, like sfs.DeadConnError, so
// every existing "abort on a dead connection" check (ContainsNonTimeoutNetError, the
// errors.As(&netErr) && !netErr.Timeout() pattern) stops the run without knowing about it.
// errors.Is(err, ErrKicked) and friends match through Unwrap.
type SessionEndedError struct {
	Cmd  string // the push, e.g. push.user.off
	Code string // the push's errorCode field, if it carried one
	Err  error  // its sentinel, e.g. ErrKicked
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

// TerminalPushError returns the SessionEndedError for a push with params that ends the session,
// or nil otherwise. ownDeviceID is the session's Login deviceId (GameConn.DeviceID), needed for
// push.account.device.del. ReadEnvelope already applies it to every frame it decodes, so read
// loops built on ReadEnvelope get the error without calling this; it exists for code that
// classifies an already-decoded ExtensionMessage itself.
func TerminalPushError(cmd string, params *sfs.SFSObject, ownDeviceID string) error {
	if e := terminalPush(cmd, params, ownDeviceID); e != nil {
		return e
	}
	return nil
}
