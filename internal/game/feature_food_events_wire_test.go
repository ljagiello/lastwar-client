package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// bWireParam is one param a wire test expects: its key, its SFS type tag and its value as the
// decoded Go value (Int int32, Long int64, UtfString string, Bool bool).
type bWireParam struct {
	key string
	typ byte
	val any
}

// bWireReq is one request a wire test expects, its command and params written as literals.
type bWireReq struct {
	cmd    string
	params []bWireParam
}

// bWireCheck asserts that got is exactly want: the commands in order, each request's key set
// (without the session's _id) and each param's type tag and value. Type tags are compared
// directly, since GetInt also accepts a Long.
func bWireCheck(t *testing.T, got []evReq, want []bWireReq) {
	t.Helper()
	var gotCmds, wantCmds []string
	for _, r := range got {
		gotCmds = append(gotCmds, r.Cmd)
	}
	for _, w := range want {
		wantCmds = append(wantCmds, w.cmd)
	}
	if !slices.Equal(gotCmds, wantCmds) {
		t.Fatalf("sent %v\nwant %v", gotCmds, wantCmds)
	}
	for i, w := range want {
		p := got[i].Params
		keys := session.ParamKeysWithoutID(p)
		var wantKeys []string
		for _, wp := range w.params {
			wantKeys = append(wantKeys, wp.key)
		}
		slices.Sort(keys)
		slices.Sort(wantKeys)
		if !slices.Equal(keys, wantKeys) {
			t.Errorf("%s #%d keys %v, want %v", w.cmd, i, keys, wantKeys)
		}
		for _, wp := range w.params {
			if v, ok := p.Get(wp.key); !ok || v.Type != wp.typ || v.Val != wp.val {
				t.Errorf("%s #%d %s = %#v (type %d, present %v), want %#v of type %d", w.cmd, i, wp.key, v.Val, v.Type, ok, wp.val, wp.typ)
			}
		}
	}
}

// bWireRun runs the feature registered as name (a literal, so a renamed feature fails here)
// against a fake that answers with reply, and returns what it sent.
func bWireRun(t *testing.T, name string, in *Init, reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) []evReq {
	t.Helper()
	f, ok := featureRegistry[name]
	if !ok {
		t.Fatalf("feature %q is not registered", name)
	}
	conn, fake := startEvFake(t, reply)
	if err := f.Run(conn, in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fake.requests()
}

// bObjArray is an SFSArray of the given objects, for fixture lists whose entries differ in shape.
func bObjArray(objs ...*sfs.SFSObject) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, o := range objs {
		a.AddSFSObject(o)
	}
	return a
}

// bInt, bLong and bStr are expected params of the three SFS types these claims use; bReq is one
// expected request.
func bInt(key string, v int32) bWireParam        { return bWireParam{key, sfs.SFSInt, v} }
func bLong(key string, v int64) bWireParam       { return bWireParam{key, sfs.SFSLong, v} }
func bStr(key string, v string) bWireParam       { return bWireParam{key, sfs.SFSUtfString, v} }
func bReq(cmd string, ps ...bWireParam) bWireReq { return bWireReq{cmd, ps} }

// TestFoodEventsWire pins the four food features' wire shape as literals from the 1.0.370 client:
//
//   - activity.mak.food.info (MsgDefines.lua:1653) {aid: Int}: ActivityMakeFoodInfoMessage.lua:7,
//     sent by ActivityListDataManager.lua:405.
//   - activity.mak.food.score.reward (MsgDefines.lua:1655) {aid, id, score: Int}:
//     ActivityMakeFoodScoreRewardMessage.lua:8-10, sent by CookingRewardBoxItem.lua:65.
//   - activity.food.party.info (MsgDefines.lua:1657) {aid: Int}: ActivityFoodPartyInfoMessage.lua:7,
//     sent by ActivityListDataManager.lua:411.
//   - activity.food.party.level.reward (MsgDefines.lua:1659) {aid, id, level: Int}:
//     ActivityFoodPartyLevelRewardMessage.lua:8-10, sent by UIActChristmasTree.lua:476.
//   - activity.food.party.v2.info (MsgDefines.lua:1663) {aid: Int}: ActivityFoodPartyV2InfoMessage.
//     lua:7, sent by ActivityListDataManager.lua:417.
//   - activity.food.party.v2.task.reward (MsgDefines.lua:1671) {activityId: Int, taskId: UtfString}:
//     ActivityFoodPartyV2TaskRewardMessage.lua:6-7, sent by BanquetAttackMonsterTaskItem.lua:167.
//   - activity.food.party.v2.level.reward (MsgDefines.lua:1664) {aid, id, level: Int}:
//     ActivityFoodPartyV2LevelRewardMessage.lua:8-10, sent by BanquetAttackMonsterLevelRewardItem.
//     lua:108.
//   - activity.food.party.v2.extra.reward (MsgDefines.lua:1670) {aid: Int}:
//     ActivityFoodPartyV2ExtraRewardMessage.lua:7, sent by UIBanquetFinRewardGetNewCtrl.lua:42.
//
// Activities 99003 (type 150), 99004 (type 151) and 99065 (type 160) are table activity rows.
// Each -plan twin sends only its info reads.
func TestFoodEventsWire(t *testing.T) {
	withEvNow(t, evTestNow)
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "activity.mak.food.info":
			return foodInfo(cookingEntry(450, "100", 100, 300)) // info[0].id 7
		case "activity.food.party.info":
			return foodInfo(banquetEntry(2, "1", 5)) // info[0].id 31
		case "activity.food.party.v2.info":
			e := banquetEntry(1, "", 5)
			e.PutSFSArray("extraReward", freebieReward())
			r := foodInfo(e)
			r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"801": 1}))
			return r
		}
		return evOK()
	}
	in := evInit(30, evActivity(99003), evActivity(99004), evActivity(99065))
	for _, c := range []struct {
		feature string
		want    []bWireReq
	}{
		{"food-event-boxes", []bWireReq{
			bReq("activity.mak.food.info", bInt("aid", 99003)),
			bReq("activity.mak.food.score.reward", bInt("aid", 99003), bInt("id", 7), bInt("score", 300)),
			bReq("activity.food.party.info", bInt("aid", 99004)),
			bReq("activity.food.party.level.reward", bInt("aid", 99004), bInt("id", 31), bInt("level", 2)),
		}},
		{"food-event-boxes-plan", []bWireReq{
			bReq("activity.mak.food.info", bInt("aid", 99003)),
			bReq("activity.food.party.info", bInt("aid", 99004)),
		}},
		{"banquet-v2-claims", []bWireReq{
			bReq("activity.food.party.v2.info", bInt("aid", 99065)),
			bReq("activity.food.party.v2.task.reward", bInt("activityId", 99065), bStr("taskId", "801")),
			bReq("activity.food.party.v2.level.reward", bInt("aid", 99065), bInt("id", 31), bInt("level", 1)),
			bReq("activity.food.party.v2.extra.reward", bInt("aid", 99065)),
		}},
		{"banquet-v2-claims-plan", []bWireReq{
			bReq("activity.food.party.v2.info", bInt("aid", 99065)),
		}},
	} {
		t.Run(c.feature, func(t *testing.T) {
			bWireCheck(t, bWireRun(t, c.feature, in, reply), c.want)
		})
	}
}
