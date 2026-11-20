package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestSunriseWire pins what sunrise-foundation sends, as literals from the 1.0.370 client:
//   - sunrise.info {activityId: Int} (MsgDefines.lua:582; SunriseInfoMessage.lua:4-7;
//     ActivityListDataManager.lua:434)
//   - sunrise.receive.reward {activityId: Int} (MsgDefines.lua:583; SunriseReceiveRewardMessage.lua:4-7;
//     UILWSunriseFoundation.lua:321)
//
// Activity 94312 is type 183 (SunriseFoundation) in table activity 40413.
func TestSunriseWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "sunrise.info" {
			return obj("activityId", 94312, "unlock", 0, "rewardArr", []*sfs.SFSObject{
				obj("level", 2, "rewardFlag", 0, "specialRewardFlag", 0)})
		}
		return evOK()
	})
	if err := runSunrise(conn, evInit(30, evActivity(94312))); err != nil {
		t.Fatal(err)
	}
	dCheckWire(t, fake, []dWire{
		{"sunrise.info", map[string]dParam{"activityId": dInt(94312)}},
		{"sunrise.receive.reward", map[string]dParam{"activityId": dInt(94312)}},
	})
}
