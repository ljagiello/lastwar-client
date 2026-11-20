package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// cityPoint is an init cityPoints entry as CityPointData.lua:21-35 reads it; rewards < 0 leaves
// the reward field out.
func cityPoint(uuid int64, typ int32, rewards int) *sfs.SFSObject {
	p := obj("uuid", uuid, "pointId", int(uuid%1000), "type", int(typ), "itemId", "1001")
	if rewards >= 0 {
		var rs []*sfs.SFSObject
		for range rewards {
			rs = append(rs, obj("type", 27, "value", obj("itemId", 630011, "rewardAdd", 5)))
		}
		arr := sfs.NewSFSArray()
		for _, r := range rs {
			arr.AddSFSObject(r)
		}
		p.PutSFSArray("reward", arr)
	}
	return p
}

func cityGarbageInit(points ...*sfs.SFSObject) *Init {
	return rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		arr := sfs.NewSFSArray()
		for _, p := range points {
			arr.AddSFSObject(p)
		}
		raw.PutSFSArray("cityPoints", arr)
	})
}

func noCityGarbageSpacing(t *testing.T) {
	t.Helper()
	orig := cityGarbageSpacing
	cityGarbageSpacing = 0
	t.Cleanup(func() { cityGarbageSpacing = orig })
}

func TestCityGarbageIsOptIn(t *testing.T) { assertCityGapFeature(t, cityGarbageFeature) }

func TestCityGarbageClaimsRewardPiles(t *testing.T) {
	noCityGarbageSpacing(t)
	buf := captureVisitorLog(t)
	in := cityGarbageInit(
		cityPoint(101, cityPointGarbageReward, 1),  // claim
		cityPoint(102, 1, 0),                       // Garbage (type 1): not yet picked, no pile
		cityPoint(103, cityPointMonsterReward, 1),  // counted only
		cityPoint(104, cityPointGarbageReward, 0),  // empty reward
		cityPoint(105, cityPointGarbageReward, -1), // no reward field
		cityPoint(106, cityPointGarbageReward, 2),  // claim
	)
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runCityGarbage(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	var uuids []int64
	for _, r := range fake.requests() {
		if r.Cmd != cityGarbageCmd {
			t.Errorf("sent %s, want only %s", r.Cmd, cityGarbageCmd)
		}
		v, ok := r.Params.Get("uuid")
		if !ok || v.Type != sfs.SFSLong {
			t.Errorf("params %s, want uuid Long", r.Params.StringRedacted())
		}
		uuids = append(uuids, r.Params.GetLong("uuid"))
	}
	if !slices.Equal(uuids, []int64{101, 106}) {
		t.Errorf("claimed %v, want [101 106]", uuids)
	}
	if want := "garbageReward=4 monsterReward=1 garbageWithoutReward=2 points=6"; !strings.Contains(buf.String(), want) {
		t.Errorf("log missing %q:\n%s", want, buf.String())
	}
}

func TestCityGarbageStopsOnAnError(t *testing.T) {
	noCityGarbageSpacing(t)
	captureVisitorLog(t)
	in := cityGarbageInit(cityPoint(101, cityPointGarbageReward, 1), cityPoint(106, cityPointGarbageReward, 1))
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardErr("E000000") })
	if err := runCityGarbage(conn, in); err == nil {
		t.Error("run = nil, want the errorCode (a locked fog tile, say)")
	}
	if got := fake.cmds(); len(got) != 1 {
		t.Errorf("sent %v, want one claim, then stop", got)
	}
}

func TestCityGarbageNothingToClaim(t *testing.T) {
	captureVisitorLog(t)
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	for name, in := range map[string]*Init{
		"no init":         {},
		"no cityPoints":   rewardInitWithDay(25, func(*sfs.SFSObject) {}),
		"only monsters":   cityGarbageInit(cityPoint(103, cityPointMonsterReward, 1)),
		"garbage pending": cityGarbageInit(cityPoint(102, 1, 0)),
	} {
		if err := runCityGarbage(conn, in); err != nil || len(fake.cmds()) != 0 {
			t.Errorf("%s: err %v, sent %v", name, err, fake.cmds())
		}
	}
}

// The client walks cityPoints with pairs, so an object keyed by uuid reads like the array.
func TestCityGarbageReadsKeyedCityPoints(t *testing.T) {
	noCityGarbageSpacing(t)
	captureVisitorLog(t)
	in := rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		m := sfs.NewSFSObject()
		m.PutSFSObject("101", cityPoint(101, cityPointGarbageReward, 1))
		raw.PutSFSObject("cityPoints", m)
	})
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runCityGarbage(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{cityGarbageCmd}) {
		t.Errorf("sent %v, want one claim", got)
	}
}
