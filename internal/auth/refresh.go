package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// TokenRefresh is what RefreshTokens needs to trade a refresh token for a fresh access token
// through GSL getserverlist.php opt=refresh.
type TokenRefresh struct {
	HTTPClient *http.Client
	GateHost   string         // the check-version host that answered
	RSAPub     *rsa.PublicKey // from that check-version reply's resMsg
	DeviceID   string         // the device the token pair was issued to
	IOSMode    bool           // sends platform=iOS, as the iOS app that issued the session does
	Zone       string         // the session's zone, sent as `zone` like the client's cached SERVER_ZONE
	GameUid    string
	RefreshTok string
}

func (r TokenRefresh) String() string   { return "[REDACTED TokenRefresh]" }
func (r TokenRefresh) GoString() string { return r.String() }

// LogValue makes TokenRefresh satisfy slog.LogValuer, like every other credential-bearing type here.
func (r TokenRefresh) LogValue() slog.Value { return slog.StringValue(r.String()) }

// Tokens is what an opt=refresh reply hands back. RefreshTok is "" when the reply carried no rt:
// the caller keeps the one it sent. The times are GSL's at.time/rt.time, unix seconds of issue.
type Tokens struct {
	AccessTok, RefreshTok         string
	AccessTokTime, RefreshTokTime int64
}

func (t Tokens) String() string   { return "[REDACTED Tokens]" }
func (t Tokens) GoString() string { return t.String() }

// LogValue makes Tokens satisfy slog.LogValuer, like every other credential-bearing type here.
func (t Tokens) LogValue() slog.Value { return slog.StringValue(t.String()) }

// RefreshTokens runs GSL opt=refresh, what the 1.0.364 client does when its at is empty (after a
// Login E011 clears it) or at.time is 30 days old (gsl.ChooseOpt). The client stores whatever
// non-empty at and rt arrive. Only the tokens are taken from the reply: the caller keeps its game
// server, which LookupGameServer and the Login's serverInfo redirect already track.
//
// A non-zero code fails, 211/212 wrapping gsl.ErrReauthNeeded. An empty server list with code 0
// doesn't: the token is what this call is for.
func RefreshTokens(r TokenRefresh) (Tokens, error) {
	if r.RefreshTok == "" {
		return Tokens{}, errors.New("GSL refresh: no refresh token")
	}
	platform := loginBuild(r.IOSMode, "", "").Platform
	slog.Info("GSL getserverlist: refreshing the access token", "opt", "refresh", "platform", platform, "zone", r.Zone, "gameUid", r.GameUid)
	lsr, err := gsl.GetServerListAs(r.HTTPClient, r.GateHost, r.RSAPub, platform, r.DeviceID, gsl.GSLOpt{Opt: "refresh", Rt: r.RefreshTok}, r.Zone, r.GameUid)
	if err != nil {
		return Tokens{}, fmt.Errorf("GSL refresh: %w", err)
	}
	if err := lsr.Err(); err != nil && !errors.Is(err, gsl.ErrEmptyServerList) {
		return Tokens{}, fmt.Errorf("GSL refresh: %w", err)
	}
	if lsr.At == nil || lsr.At.Token.String() == "" {
		return Tokens{}, fmt.Errorf("GSL refresh: code=%q but the reply carried no access token", lsr.Code)
	}
	t := Tokens{AccessTok: lsr.At.Token.String(), AccessTokTime: int64(lsr.At.Time.Int("at.time"))}
	if lsr.Rt != nil && lsr.Rt.Token.String() != "" {
		t.RefreshTok, t.RefreshTokTime = lsr.Rt.Token.String(), int64(lsr.Rt.Time.Int("rt.time"))
	}
	slog.Info("GSL refresh: fresh access token", "accessTokenLen", len(t.AccessTok), "accessTokenTime", t.AccessTokTime,
		"replyHasRefreshToken", t.RefreshTok != "", "refreshTokenRotated", t.RefreshTok != "" && t.RefreshTok != r.RefreshTok)
	return t, nil
}
