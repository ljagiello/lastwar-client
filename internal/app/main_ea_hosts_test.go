package app

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/testutil"
)

// TestRunCrossServerTestCheckVersionHostFollowsTheDeviceBucket: the reconnect passes its deviceId
// to check-version, so a device in the client's early-access bucket (GrayUtils.GetPercentageFromMd5
// <= 5, Assembly-CSharp.decompiled.cs:14131-14141) asks online_ea_CheckVersionHostList and any
// other the online list (:13831-13833). The built-in lists stay in place; the in-memory transport
// answers by path and never dials, so no real host is contacted.
func TestRunCrossServerTestCheckVersionHostFollowsTheDeviceBucket(t *testing.T) {
	for _, c := range []struct {
		name, deviceID, wantHost string
	}{
		{"bucket 4", "device-21_n3d", "lastwar-serverlist-ea-us-aws.lastwargame.com"},
		{"bucket 97", "0123456789ABCDEF0123456789ABCDEF_n3d", "lastwar-serverlist-cf.lastwarapp.net"},
	} {
		t.Run(c.name, func(t *testing.T) {
			builtin := slices.Clone(gsl.CheckVersionHosts)
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, lookupReply())
			gsl.CheckVersionHosts = builtin // the early-access pick applies only to the built-in online list

			var mu sync.Mutex
			var hosts []string
			base := client.Transport
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "getlsu3dversion.php") {
					mu.Lock()
					hosts = append(hosts, r.URL.Host)
					mu.Unlock()
				}
				return base.RoundTrip(r)
			})
			dial, _ := recordingDial(nil)

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: lookupGateways, port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
					deviceID: c.deviceID, configSavePath: t.TempDir() + "/session.json", httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			mu.Lock()
			defer mu.Unlock()
			if len(hosts) == 0 || hosts[0] != c.wantHost {
				t.Errorf("check-version hosts asked = %v, want %s first", hosts, c.wantHost)
			}
		})
	}
}
