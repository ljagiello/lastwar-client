package app

import (
	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// writeOwnDeviceSession implements -own-device-session: after the -email flow has bound this
// client's own device to the account, it trades the persisted loginKey for the device's own token
// pair and writes a ready-to-use session config (Android identity, since that is what this device
// presents to GSL) to path. Token values are never logged, only their lengths.
func writeOwnDeviceSession(httpClient *http.Client, path string) error {
	s, err := auth.BootstrapOwnDevice(httpClient)
	if err != nil {
		return err
	}
	cfg := sessionConfigFromOwnDevice(s)
	if err := SaveSessionConfig(cfg, path); err != nil {
		return err
	}
	slog.Info("wrote own-device session config", "path", path, "ip", cfg.IP, "port", cfg.Port, "zone", cfg.Zone,
		"accessTokenLen", len(cfg.AccessToken), "accessTokenTime", cfg.AccessTokenTime,
		"refreshTokenLen", len(cfg.RefreshToken), "refreshTokenTime", cfg.RefreshTokenTime)
	return nil
}

func sessionConfigFromOwnDevice(s *auth.OwnDeviceSession) *SessionConfig {
	return &SessionConfig{
		IP: s.IP, Port: s.Port, Zone: s.Zone, GameUid: s.GameUid, DeviceID: s.DeviceID,
		AccessToken: s.AccessTok, AccessTokenTime: s.AccessTokTime,
		RefreshToken: s.RefreshTok, RefreshTokenTime: s.RefreshTokTime,
		AppVersion: gsl.AppVersion, VersionCode: gsl.VersionCode,
	}
}
