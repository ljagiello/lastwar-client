package main

import (
	"strings"
	"testing"

	"lastwar-client/internal/auth"
	"lastwar-client/internal/sfs"
)

// TestAcceptedLoginRecordsCapturedDevice: an iOS capture's device fields are recorded for the
// reconnect to replay (1.0.370 audit B15); an Android capture's are not, and the summary names the
// recorded fields without their values.
func TestAcceptedLoginRecordsCapturedDevice(t *testing.T) {
	login := func(platform string) []byte {
		params := sfs.NewSFSObject()
		params.PutUtfString("deviceId", testDevice)
		params.PutUtfString("gameUid", "1000000000000783")
		params.PutUtfString("platform", platform)
		params.PutUtfString("at", testToken)
		for k, v := range map[string]string{
			"phone_model": "iPhone15,2", "osVersion": "iOS 18.1", "mt": "mt-1", "idfv": "IDFV-SECRET", "distinct_id": "d-1",
			"fromCountry": "PL", "lang": "pl", "phone_screen": "1179*2556", "afuid": "af-1", "firebaseId": "fb-1",
		} {
			params.PutUtfString(k, v)
		}
		req := sfs.NewSFSObject()
		req.PutUtfString("zn", "APS783")
		req.PutSFSObject("p", params)
		return framedEnvelope(t, 0, 1, req)
	}
	ok := sfs.NewSFSObject()
	ok.PutShort("rs", 0)
	reply := framedEnvelope(t, 0, 1, ok)

	cfg, reason := acceptedLogin(login("0"), reply)
	if cfg == nil {
		t.Fatalf("acceptedLogin: %s", reason)
	}
	want := auth.CapturedDevice{
		PhoneModel: "iPhone15,2", OSVersion: "iOS 18.1", HandSetInfo: "mt-1", Idfv: "IDFV-SECRET", DistinctID: "d-1",
		FromCountry: "PL", Lang: "pl", PhoneScreen: "1179*2556", Afuid: "af-1", FirebaseID: "fb-1",
	}
	if cfg.Device != want {
		t.Error("the recorded device differs from the captured Login's fields")
	}
	const wantFields = "phone_model,osVersion,mt,idfv,distinct_id,fromCountry,lang,phone_screen,afuid,firebaseId"
	if got := strings.Join(capturedDeviceFields(cfg.Device), ","); got != wantFields {
		t.Errorf("summary fields = %s, want %s", got, wantFields)
	}
	if cfg, _ := acceptedLogin(login("1"), reply); cfg == nil || !cfg.Device.IsZero() {
		t.Error("an Android capture recorded device fields")
	}
}
