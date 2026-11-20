package auth

import (
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/testutil"
)

// TestBootstrapOwnDeviceSendsTheBoundZone: the opt=login trade sends the bound role's zone, which
// the client's role picker caches before that call (UIRoleLoginView.lua:110-120;
// Assembly-CSharp.decompiled.cs:77411, 77437; 1.0.370 audit B80). A device bound before the zone was
// recorded sends "" as before.
func TestBootstrapOwnDeviceSendsTheBoundZone(t *testing.T) {
	for _, zone := range []string{"APS72", ""} {
		t.Run("zone="+zone, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ident, err := LoadOrCreateDeviceIdentity()
			if err != nil {
				t.Fatal(err)
			}
			for _, save := range []func(string) error{ident.SaveLoginKey, ident.SaveGameUid} {
				if err := save("x-1"); err != nil {
					t.Fatal(err)
				}
			}
			if zone != "" {
				if err := ident.SaveZone(zone); err != nil {
					t.Fatal(err)
				}
			}
			client, forms := testutil.UseDecryptingGSL(t, gsl.LoginServerListRespon{
				Code:       "0",
				ServerList: []gsl.LoginServerInfo{{ID: "72", IP: "h.example", Port: "10072", Zone: "APS72", GameUid: "x-1"}},
				At:         &gsl.LoginToken{Token: "at-1", Time: "10"},
			})
			if _, err := BootstrapOwnDevice(client, true); err != nil {
				t.Fatal(err)
			}
			got := forms()
			if len(got) != 1 || got[0].Get("opt") != "login" || got[0].Get("zone") != zone {
				t.Errorf("GSL forms = %v, want one opt=login with zone=%q", got, zone)
			}
		})
	}
}
