package auth

import (
	"encoding/json"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
)

func gslReply(t *testing.T, body string) *gsl.LoginServerListRespon {
	t.Helper()
	var lsr gsl.LoginServerListRespon
	if err := json.Unmarshal([]byte(body), &lsr); err != nil {
		t.Fatal(err)
	}
	return &lsr
}

// TestOwnDeviceSessionFromPicksTheRole mirrors the live GSL opt=login reply's shape (2026-10-04):
// the entry carrying this account's gameUid wins over serverList[0], and at/rt plus their times
// are carried through.
func TestOwnDeviceSessionFromPicksTheRole(t *testing.T) {
	lsr := gslReply(t, `{"code":0,"lastLoggedServer":"783",
		"at":{"token":"at-1","time":1791176197},"rt":{"token":"rt-1","time":1791176198},
		"serverList":[
			{"id":"72","ip":"a.example|b.example","port":"10072","zone":"APS72","gameUid":"999"},
			{"id":"783","ip":"c.example|d.example","port":"10783","zone":"APS783","gameUid":"1000000000000783"}]}`)
	s, err := ownDeviceSessionFrom(lsr, "dev_n3d", "1000000000000783")
	if err != nil {
		t.Fatal(err)
	}
	if s.Zone != "APS783" || s.Port != 10783 || s.IP != "c.example|d.example" {
		t.Errorf("server = %s %s:%d, want APS783 c.example|d.example:10783", s.Zone, s.IP, s.Port)
	}
	if s.AccessTok != "at-1" || s.AccessTokTime != 1791176197 || s.RefreshTok != "rt-1" || s.RefreshTokTime != 1791176198 {
		t.Errorf("tokens not carried through: %#v", *s) // GoString is redacted by design
	}
}

func TestOwnDeviceSessionFromRejects(t *testing.T) {
	cases := map[string]string{
		"gsl code 211":  `{"code":211,"serverList":[{"id":"1"}]}`,
		"no at":         `{"code":0,"serverList":[{"id":"1","ip":"h","port":"1","zone":"APS1"}]}`,
		"no serverList": `{"code":0,"at":{"token":"at-1","time":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ownDeviceSessionFrom(gslReply(t, body), "dev", "uid"); err == nil {
				t.Error("expected an error")
			} else if name == "gsl code 211" && !strings.Contains(err.Error(), "211") {
				t.Errorf("error %q should name the GSL code", err)
			}
		})
	}
}
