package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestHeroCountdownWire: a hero-countdown building (LW_BUILD_HERO_COUNTDOWN 10222000,
// EnumType.lua:3728) with a uuid above 2^32 whose fixCityHeroET passed a minute ago.
//   - free.building.fix.city.hero (MsgDefines.lua:1955; sent at BuildHeroCountdownManager.lua:97)
//     {uuid: Long} (FreeBuildingFixCityHeroMessage.lua:4-7).
func TestHeroCountdownWire(t *testing.T) {
	const uuid = int64(1)<<40 + 11
	in := rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		v, _ := raw.Get("building_new")
		b := NewTestBuildingSFS(uuid, 10222000, 1)
		b.PutLong("fixCityHeroET", time.Now().Add(-time.Minute).UnixMilli())
		v.Val.(*sfs.SFSArray).AddSFSObject(b)
	})
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "free.building.fix.city.hero" {
			return rewardOK()
		}
		return rewardErr("unexpected command")
	})
	if err := runHeroCountdown(conn, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aRewardSent(fake), []aWireReq{
		{cmd: "free.building.fix.city.hero", params: []aWireParam{{"uuid", sfs.SFSLong, uuid}}},
	})
}
