package auth

import (
	"errors"
	"fmt"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// OwnDeviceSession is what GSL hands this client's own device once it is bound to an account: an
// access/refresh token pair plus the game server of the account's role. Unlike a session config
// captured from the real app, these tokens belong to this device alone, so refreshing them (GSL
// opt=refresh) couldn't invalidate the real app's tokens or the other way round.
//
// EXPERIMENTAL, confirmed live 2026-10-04 only up to the token exchange: GSL opt=login returned an
// at/rt pair (at.time is the issue time, in unix seconds) and the role's server (APS783, its three
// gateway hostnames). The base-zone Login with that token and this client's Android identity was
// then rejected with ec=28/E005, as in docs/live-validation.mdx's reconnect-wall row 2. A missing
// shumeiBoxId is not the cause: the captured iOS session logs in fine without one, tested the same
// night. The actual gate is still open.
type OwnDeviceSession struct {
	DeviceID              string
	GameUid               string
	AccessTok, RefreshTok string
	// AccessTokTime/RefreshTokTime are GSL's at.time/rt.time. The 1.0.364 client treats at.time
	// as an issue time and refreshes once it is 30 days old (A-CS:77155-77170).
	AccessTokTime, RefreshTokTime int64
	IP                            string // "|"-delimited gateway hostnames, as GSL lists them
	Port                          int
	Zone                          string
}

func (s OwnDeviceSession) String() string   { return "[REDACTED OwnDeviceSession]" }
func (s OwnDeviceSession) GoString() string { return s.String() }

// BootstrapOwnDevice exchanges the loginKey that the -email verification flow persisted for this
// device's first token pair (GSL opt=login, exactly what the real client does after an account
// switch), and picks the server of the account's role from the reply. The loginKey is left on disk:
// the real client discards it after this call, but keeping it costs nothing if the server accepts
// it again, and saves a second email code if it does.
func BootstrapOwnDevice(httpClient *http.Client) (*OwnDeviceSession, error) {
	ident, err := LoadOrCreateDeviceIdentity()
	if err != nil {
		return nil, err
	}
	if ident.LoginKey == "" || ident.GameUid == "" {
		return nil, errors.New("own-device bootstrap: no persisted loginKey/gameUid -- run the -email verification flow first, with the same LASTWAR_STATE_DIR")
	}
	cv, gateHost, err := gsl.CheckVersion(httpClient)
	if err != nil {
		return nil, err
	}
	pub, err := crypto.ParseRSAPubKeyFromDER(cv.ResMsg.String())
	if err != nil {
		return nil, err
	}
	lsr, err := gsl.GetServerList(httpClient, gateHost, pub, ident.DeviceID, gsl.GSLOpt{Opt: "login", LoginKey: ident.LoginKey}, "", ident.GameUid)
	if err != nil {
		return nil, err
	}
	return ownDeviceSessionFrom(lsr, ident.DeviceID, ident.GameUid)
}

// ownDeviceSessionFrom validates a GSL reply (code 0, a non-empty at) and picks the server entry
// carrying this account's gameUid, falling back to lastLoggedServer and then the first entry, the
// same preference order as the real client's GetLastLoggedServerInfo() ?? serverList[0].
func ownDeviceSessionFrom(lsr *gsl.LoginServerListRespon, deviceID, gameUid string) (*OwnDeviceSession, error) {
	// 211: at/rt/loginKey rejected (client clears all three); 212: also the uid/server mapping;
	// 201: not available; 213: envelope crypto error (1.0.364 C#, A-CS:12665-12705).
	if code := lsr.Code.String(); code != "" && code != "0" {
		return nil, fmt.Errorf("GSL rejected the request: code=%s (211/212 mean the loginKey/refresh token is no longer valid: re-run the -email flow)", code)
	}
	if lsr.At == nil || lsr.At.Token.String() == "" {
		return nil, errors.New("GSL reply carried no access token")
	}
	if len(lsr.ServerList) == 0 {
		return nil, errors.New("GSL reply carried no server list")
	}
	srv := lsr.ServerList[0]
	for _, s := range lsr.ServerList {
		if s.GameUid.String() == gameUid {
			srv = s
			break
		}
		if s.ID.String() == lsr.LastLoggedServer.String() {
			srv = s
		}
	}
	slog.Info("own device: GSL server selected", "id", srv.ID, "zone", srv.Zone, "ip", srv.IP, "port", srv.Port,
		"serverListLen", len(lsr.ServerList), "lastLoggedServer", lsr.LastLoggedServer)
	s := &OwnDeviceSession{
		DeviceID:      deviceID,
		GameUid:       gameUid,
		AccessTok:     lsr.At.Token.String(),
		AccessTokTime: int64(lsr.At.Time.Int("at.time")),
		IP:            srv.IP.String(),
		Port:          srv.Port.Int("port"),
		Zone:          srv.Zone.String(),
	}
	if g := srv.GameUid.String(); g != "" {
		s.GameUid = g
	}
	if lsr.Rt != nil {
		s.RefreshTok = lsr.Rt.Token.String()
		s.RefreshTokTime = int64(lsr.Rt.Time.Int("rt.time"))
	}
	return s, nil
}
