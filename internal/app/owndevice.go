package app

import (
	"lastwar-client/internal/auth"
	"log/slog"
	"net/http"
)

// writeOwnDeviceSession implements -own-device-session: after the -email flow has bound this
// client's own device to the account, it trades the persisted loginKey for the device's own token
// pair and writes a ready-to-use session config to path. iosMode is -cs-ios: the trade and the
// config use the iOS identity, otherwise Android. Token values are never logged, only their lengths.
func writeOwnDeviceSession(httpClient *http.Client, path string, iosMode bool) error {
	s, err := auth.BootstrapOwnDevice(httpClient, iosMode)
	if err != nil {
		return err
	}
	cfg := sessionConfigFromOwnDevice(s)
	if err := SaveSessionConfig(cfg, path); err != nil {
		return err
	}
	slog.Info("wrote own-device session config", "path", path, "ip", cfg.IP, "port", cfg.Port, "zone", cfg.Zone,
		"iosMode", cfg.IOSMode, "appVersion", cfg.AppVersion, "versionCode", cfg.VersionCode,
		"accessTokenLen", len(cfg.AccessToken), "accessTokenTime", cfg.AccessTokenTime,
		"refreshTokenLen", len(cfg.RefreshToken), "refreshTokenTime", cfg.RefreshTokenTime)
	return nil
}

func sessionConfigFromOwnDevice(s *auth.OwnDeviceSession) *SessionConfig {
	return &SessionConfig{
		IP: s.IP, Port: s.Port, Zone: s.Zone, GameUid: s.GameUid, DeviceID: s.DeviceID,
		AccessToken: s.AccessTok, AccessTokenTime: s.AccessTokTime,
		RefreshToken: s.RefreshTok, RefreshTokenTime: s.RefreshTokTime,
		IOSMode:    s.IOSMode,
		AppVersion: s.AppVersion, VersionCode: s.VersionCode,
	}
}
