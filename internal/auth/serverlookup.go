package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
	"time"
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
}

func (l ServerLookup) String() string   { return "[REDACTED ServerLookup]" }
func (l ServerLookup) GoString() string { return l.String() }

// LogValue makes ServerLookup satisfy slog.LogValuer, like every other credential-bearing type here.
func (l ServerLookup) LogValue() slog.Value { return slog.StringValue(l.String()) }

// GameServer is the game server GSL lists for a role.
type GameServer struct {
	IP   string // "|"-delimited gateway hosts, all on Port
	Port int
	Zone string
}

// LookupGameServer asks GSL getserverlist.php for the game server of l.GameUid's role. The 1.0.364
// client makes this call on every cold start: CheckResVersionState clears the cached
// SERVER_IP/PORT (A-CS:9268), so StartConnect always runs GetServerList (A-CS:7230-7246). The
// address it returns follows zone moves and port changes that a captured address doesn't.
//
// The opt is gsl.ChooseOpt over what the lookup holds, a gameUid and an access token but no refresh
// token, so it is opt=fix. A session's refresh token is never sent: the caller keeps its access
// token, so an opt=refresh that rotated the pair would strand both.
//
// Only the address is taken from the reply. The caller keeps its access token, as the client does
// on a serverInfo redirect. Whether GSL issues or rotates tokens on opt=fix for a session the iOS
// app issued is untested (static-analysis-only), so any token in the reply is only compared with
// AccessTok, and the result is logged.
func LookupGameServer(l ServerLookup) (GameServer, error) {
	if l.GameUid == "" {
		// Without a gameUid, gsl.ChooseOpt picks opt=new, which asks GSL for a brand-new account.
		return GameServer{}, errors.New("GSL server lookup: no gameUid")
	}
	opt := gsl.ChooseOpt(gsl.OptState{GameUid: l.GameUid, AccessTok: l.AccessTok}, time.Now())
	platform := loginBuild(l.IOSMode, "", "").Platform
	slog.Info("GSL getserverlist: looking up the game server", "opt", opt.Opt, "platform", platform, "zone", l.Zone, "gameUid", l.GameUid)
	lsr, err := gsl.GetServerListAs(l.HTTPClient, l.GateHost, l.RSAPub, platform, l.DeviceID, opt, l.Zone, l.GameUid)
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
	replyTok := ""
	if lsr.At != nil {
		replyTok = lsr.At.Token.String()
	}
	replyHasRt := lsr.Rt != nil && lsr.Rt.Token != ""
	slog.Info("GSL server lookup: game server found", "id", srv.ID, "ip", s.IP, "port", s.Port, "zone", s.Zone,
		"serverListLen", len(lsr.ServerList), "lastLoggedServer", lsr.LastLoggedServer,
		"replyHasAccessToken", replyTok != "", "replyAccessTokenMatches", replyTok != "" && replyTok == accessTok,
		"replyHasRefreshToken", replyHasRt)
	if replyTok != "" && replyTok != accessTok {
		slog.Warn("GSL server lookup: the reply carries a different access token; keeping the session's. " +
			"If the Login now fails with E011, GSL rotated it and the session needs a fresh capture")
	}
	return s, nil
}
