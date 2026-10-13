package auth

import (
	"encoding/json"
	"errors"
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
			} else if name == "gsl code 211" && (!strings.Contains(err.Error(), "211") || !errors.Is(err, gsl.ErrReauthNeeded)) {
				t.Errorf("error %q should name the GSL code and wrap gsl.ErrReauthNeeded", err)
			}
		})
	}
}

// TestOwnDeviceSessionFromRefusesAnotherRole: when GSL lists no entry for the requested gameUid
// (say -server picked a role GSL's opt=login reply doesn't list), PickServer's fallback entry
// belongs to another role, and its gameUid must not replace the requested one in the config. An
// entry without a gameUid can't be told apart, so it is used, keeping the requested gameUid.
func TestOwnDeviceSessionFromRefusesAnotherRole(t *testing.T) {
	other := gslReply(t, `{"code":0,"lastLoggedServer":"783","at":{"token":"at-1","time":1},
		"serverList":[{"id":"783","ip":"c.example","port":"10783","zone":"APS783","gameUid":"uid-783"}]}`)
	if _, err := ownDeviceSessionFrom(other, "dev", "uid-72"); err == nil || !strings.Contains(err.Error(), "uid-72") {
		t.Errorf("err = %v, want a refusal naming the requested gameUid", err)
	}
	unnamed := gslReply(t, `{"code":0,"at":{"token":"at-1","time":1},
		"serverList":[{"id":"72","ip":"h.example","port":"10072","zone":"APS72"}]}`)
	s, err := ownDeviceSessionFrom(unnamed, "dev", "uid-72")
	if err != nil {
		t.Fatal(err)
	}
	if s.GameUid != "uid-72" || s.Zone != "APS72" {
		t.Errorf("gameUid/zone = %q/%q, want uid-72/APS72", s.GameUid, s.Zone)
	}
}
