package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestHeroExchangeRewardWire pins what hero-exchange-reward sends, as literals from the 1.0.370
// client: lw.season.hero.receive.switch.reward {activityId: Int} (MsgDefines.lua:2284;
// LwSeasonHeroReceiveSwitchRewardMessage.lua:4-7; UIHeroExchangeGuideView.lua:167), once per open
// type-323 activity whose guide reward is not received. Activities 1030002 and 1200066 are type 323
// (ActHeroLevelAndStarReplace) in table activity 40413.
func TestHeroExchangeRewardWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	received := evActivity(1030003)
	received.PutBool("receiveReward", true)
	if err := runHeroExchangeReward(conn, evInit(30, evActivity(1030002), received, evActivity(1200066))); err != nil {
		t.Fatal(err)
	}
	dCheckWire(t, fake, []dWire{ // the open activities come by id (ActivitySweep.Open)
		{"lw.season.hero.receive.switch.reward", map[string]dParam{"activityId": dInt(1030002)}},
		{"lw.season.hero.receive.switch.reward", map[string]dParam{"activityId": dInt(1200066)}},
	})
}
