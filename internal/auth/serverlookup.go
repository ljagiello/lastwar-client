package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// ServerLookup is what LookupGameServer needs to ask GSL getserverlist.php where a role's game
// server is.
type ServerLookup struct {
	HTTPClient *http.Client
	GateHost   string         // the check-version host that answered
	RSAPub     *rsa.PublicKey // from that check-version reply's resMsg
	DeviceID   string
	IOSMode    bool   // sends platform=iOS, as the iOS app that issued the session does
	Zone       string // the session's zone, sent as `zone` like the client's cached SERVER_ZONE; "" if unknown
	GameUid    string // the role to find; required
	AccessTok  string // the session's access token; never sent, only compared with any token the reply carries
	// Device is the captured iOS device; its fromCountry/lang fill the form's country/lang
	// (formDevice).
	Device CapturedDevice
}

func (l ServerLookup) String() string   { return "[REDACTED ServerLookup]" }
func (l ServerLookup) GoString() string { return l.String() }

// LogValue makes ServerLookup satisfy slog.LogValuer, like every other credential-bearing type here.
func (l ServerLookup) LogValue() slog.Value { return slog.StringValue(l.String()) }

// GameServer is the game server GSL lists for a role, and the tokens the reply carried. The client
// stores every non-empty at/rt a GSL reply hands it and logs in with them (SaveGameLoginToken,
// Assembly-CSharp.decompiled.cs:12777, 7482-7492, 1.0.370); the times are GSL's at.time/rt.time.
type GameServer struct {
	IP   string // "|"-delimited gateway hosts, all on Port
	Port int
	Zone string

	AccessTok, RefreshTok         string // "" when the reply carried none
	AccessTokTime, RefreshTokTime int64
}

func (s GameServer) String() string       { return "[REDACTED GameServer]" }
func (s GameServer) GoString() string     { return s.String() }
func (s GameServer) LogValue() slog.Value { return slog.StringValue(s.String()) }

// LookupGameServer asks GSL getserverlist.php for the game server of l.GameUid's role. The 1.0.370
// client makes this call on every cold start: CheckResVersionState clears the cached
// SERVER_IP/PORT (ClearServerNetworkInfo, Assembly-CSharp.decompiled.cs:9332, 14844-14849), so
// StartConnect always runs GetServerList (:7210-7225). The address it returns follows zone moves
// and port changes that a captured address doesn't.
//
// The request carries no opt field: what the real app sends on a cold start while it holds a fresh
// at and an rt (gsl.ChooseOpt's last row). The session's device is that app's device, and GSL ties
// its tokens to it. Confirmed live on 2026-10-10 against a captured iOS session: with no opt, GSL
// answered code 0 with the role's server (gateway hostnames, port, zone, gameUid) and no tokens.
// opt=fix, which gsl.ChooseOpt gives a client holding no rt, was rejected with code 212. A
// session's refresh token is never sent here: refreshing is RefreshTokens' job, which the caller
// runs, and saves, when the access token is missing, 30 days old, or rejected.
//
// The reply's address, and any token it carries, come back in GameServer: the client logs in with
// both. Live, a no-opt reply has carried no token.
func LookupGameServer(l ServerLookup) (GameServer, error) {
	if l.GameUid == "" {
		// Without a gameUid the server can't tell which role to list.
		return GameServer{}, errors.New("GSL server lookup: no gameUid")
	}
	opt := gsl.GSLOpt{}
	dev := formDevice(l.IOSMode, l.DeviceID, l.Device)
	slog.Info("GSL getserverlist: looking up the game server", "opt", opt.Opt, "platform", dev.Platform, "zone", l.Zone, "gameUid", l.GameUid)
	lsr, err := gsl.GetServerListFor(l.HTTPClient, l.GateHost, l.RSAPub, dev, l.DeviceID, opt, l.Zone, l.GameUid)
	if err != nil {
		return GameServer{}, fmt.Errorf("GSL server lookup: %w", err)
	}
	return gameServerFrom(lsr, l.GameUid, l.Zone, l.AccessTok)
}

// gameServerFrom validates a GSL reply (gsl.LoginServerListRespon.Err) and picks gameUid's entry
// with gsl.LoginServerListRespon.PickServer. An entry that names a different gameUid belongs to
// another role of the account, so it is refused rather than logged in to. zone is kept when the
// entry has none.
func gameServerFrom(lsr *gsl.LoginServerListRespon, gameUid, zone, accessTok string) (GameServer, error) {
	if err := lsr.Err(); err != nil {
		return GameServer{}, fmt.Errorf("GSL server lookup: %w", err)
	}
	srv := lsr.PickServer(gameUid)
	if g := srv.GameUid.String(); g != "" && g != gameUid {
		return GameServer{}, fmt.Errorf("GSL server lookup: no server list entry for gameUid %s (the fallback pick, id %s, is gameUid %s)", gameUid, srv.ID, g)
	}
	s := GameServer{IP: srv.IP.String(), Port: srv.Port.Int("port"), Zone: zone}
	if _, err := baseZoneLoginAddrs(s.IP, s.Port); err != nil {
		return GameServer{}, fmt.Errorf("GSL server lookup: %w", err)
	}
	if z := capOversizedIdentityField("zone", srv.Zone.String(), "", "GSL server lookup"); z != "" {
		s.Zone = z
	}
	if lsr.At != nil {
		s.AccessTok = capOversizedIdentityField("accessTok", lsr.At.Token.String(), "", "GSL server lookup")
		if s.AccessTok != "" {
			s.AccessTokTime = int64(lsr.At.Time.Int("at.time"))
		}
	}
	if lsr.Rt != nil {
		s.RefreshTok = capOversizedIdentityField("refreshTok", lsr.Rt.Token.String(), "", "GSL server lookup")
		if s.RefreshTok != "" {
			s.RefreshTokTime = int64(lsr.Rt.Time.Int("rt.time"))
		}
	}
	slog.Info("GSL server lookup: game server found", "id", srv.ID, "ip", s.IP, "port", s.Port, "zone", s.Zone,
		"serverListLen", len(lsr.ServerList), "lastLoggedServer", lsr.LastLoggedServer,
		"replyHasAccessToken", s.AccessTok != "", "replyAccessTokenMatches", s.AccessTok != "" && s.AccessTok == accessTok,
		"replyHasRefreshToken", s.RefreshTok != "")
	if s.AccessTok != "" && s.AccessTok != accessTok {
		slog.Warn("GSL server lookup: the reply carries a different access token; the Login uses it, as the real client does (SaveGameLoginToken). " +
			"If the Login then fails with E011, a session with a refresh token refreshes, a captured one needs a fresh capture")
	}
	return s, nil
}

// formDevice is the device part of this identity's getserverlist.php form (gsl.FormDevice). An iOS
// session sends the captured device's fromCountry and lang, which its Login sends too; without a
// capture both stay the "US"/"en" this client has always sent.
func formDevice(iosMode bool, deviceID string, dev CapturedDevice) gsl.FormDevice {
	f := gsl.FormDevice{Platform: loginBuild(iosMode, "", "").Platform}
	if iosMode {
		dev = dev.encodable()
		f.Country, f.Lang = dev.FromCountry, dev.Lang
	}
	return f
}
