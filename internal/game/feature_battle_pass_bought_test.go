package game

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// bpBoughtTask is a taskArr entry {taskId, state, payExp?, payReward?}; payItem 0 leaves payReward
// out, payExp < 0 leaves payExp out.
func bpBoughtTask(id string, state int32, payExp int32, payItem int32, payNum int32) *sfs.SFSObject {
	tk := sfs.NewSFSObject()
	tk.PutUtfString("taskId", id)
	tk.PutInt("state", state)
	if payExp >= 0 {
		tk.PutInt("payExp", payExp)
	}
	if payItem != 0 {
		v := sfs.NewSFSObject()
		v.PutInt("id", payItem)
		v.PutInt("num", payNum)
		r := sfs.NewSFSObject()
		r.PutInt("type", rewardTypeGoods)
		r.PutSFSObject("value", v)
		l := sfs.NewSFSArray()
		l.AddSFSObject(r)
		tk.PutSFSArray("payReward", l)
	}
	return tk
}

func bpBoughtInfo(unlock, highUnlock int32, tasks ...*sfs.SFSObject) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	bp := sfs.NewSFSObject()
	bp.PutInt("level", 1)
	bp.PutInt("unlock", unlock)
	bp.PutInt("high_unlock", highUnlock)
	info.PutSFSObject("battlePass", bp)
	ta := sfs.NewSFSArray()
	for _, t := range tasks {
		ta.AddSFSObject(t)
	}
	info.PutSFSArray("taskArr", ta)
	return info
}

// B59: with a bought track, a type-27 task in state 2 whose wire payExp is non-zero is claimed
// (ActBattlePassInfo.lua:425-426; UIBattlePassNormalNewTaskCell.lua:82-103, 255-266); without one,
// or with payExp 0, it is not.
func TestBattlePassClaimsBoughtPassStateTwoTasks(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name               string
		unlock, highUnlock int32
		payExp             int32
		want               bool
	}{
		{"normal track bought", 1, 0, 5, true},
		{"high track bought", 0, 1, 5, true},
		{"nothing bought", 0, 0, 5, false},
		{"premium exp already taken", 1, 0, 0, false},
		{"no payExp on the wire", 1, 0, -1, false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == battlePassInfoCmd {
				return bpBoughtInfo(c.unlock, c.highUnlock, bpBoughtTask("301", 2, c.payExp, 0, 0))
			}
			return evOK()
		})
		if err := runBattlePass(conn, evInit(30, evActivity(19))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := fake.only(battlePassTaskCmd)
		if (len(got) == 1) != c.want {
			t.Errorf("%s: task claims %d, want claimed=%v", c.name, len(got), c.want)
			continue
		}
		if c.want {
			if v, _ := got[0].Params.Get("taskId"); v.Type != sfs.SFSInt || got[0].Params.GetInt("taskId") != 301 {
				t.Errorf("%s: taskId %#v, want Int 301", c.name, v)
			}
		}
	}
}

// B59: in a BattlePass_new reply (activityType 142) payExp is the count of the activity's score
// item (para_5; 5038 -> 620011) in the task's payReward, never the wire field
// (ActBattlePassInfo.lua:103-135, 244-249).
func TestBattlePassNewStateTwoTaskUsesTheScoreItem(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name      string
		replyID   int32 // 0: the reply names no activity
		task      *sfs.SFSObject
		wantClaim bool
	}{
		{"score item in payReward", 5038, bpBoughtTask("t1", 2, -1, 620011, 3), true},
		{"another item in payReward", 5038, bpBoughtTask("t1", 2, 9, 620008, 3), false},
		{"no payReward, wire payExp set", 5038, bpBoughtTask("t1", 2, 9, 0, 0), false},
		{"reply without activityId", 0, bpBoughtTask("t1", 2, -1, 620011, 3), false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "hero.event.info.get" {
				info := bpBoughtInfo(1, 0, c.task)
				info.PutInt("activityType", actTypeBattlePassNew)
				if c.replyID != 0 {
					info.PutInt("activityId", c.replyID)
				}
				return info
			}
			return evOK()
		})
		if err := runBattlePass(conn, evInit(30, evActivity(5038))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := fake.only(battlePassV2TaskCmd)
		if (len(got) == 1) != c.wantClaim {
			t.Errorf("%s: bp.task.reward sent %d times, want claimed=%v", c.name, len(got), c.wantClaim)
		}
	}
}

// B59 on a view that has a high tier (final-r3 prod m6, wp8-r1 m4): activity 99021 is a
// BattlePass_new (142) row with showUiType 5 and para_5 653008 in tables 40413, a
// UIBattlePassNewYear-family view whose task cell claims a state-2 task when either track is bought
// (UIBattlePassTaskCellNewYear.lua:248: the purchase popup only for unlock == 0 and high_unlock ==
// 0). Only the high track bought: the task is claimed. Neither bought: it is not.
func TestBattlePassHighTrackOnAHighTierView(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name               string
		unlock, highUnlock int32
		want               bool
	}{
		{"high track bought", 0, 1, true},
		{"nothing bought", 0, 0, false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "hero.event.info.get" {
				info := bpBoughtInfo(c.unlock, c.highUnlock, bpBoughtTask("t1", 2, -1, 653008, 4))
				info.PutInt("activityType", 142)
				info.PutInt("activityId", 99021)
				return info
			}
			return evOK()
		})
		if err := runBattlePass(conn, evInit(30, evActivity(99021))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := fake.only("bp.task.reward"); (len(got) == 1) != c.want {
			t.Errorf("%s: bp.task.reward sent %d times, want claimed=%v", c.name, len(got), c.want)
		}
	}
}

// B59: a bought high tier's reached row with special2State present and 0 sends claim-all, only for a
// pass with a high tier (highExchangeId set, HaveHighPayStage, ActBattlePassInfo.lua:398-400,
// 622-624) on a general view; a missing special2State, no high tier, no high_unlock, or a login
// pass view (showUiType 2) does not.
func TestBattlePassHighTierRows(t *testing.T) {
	withEvNow(t, evTestNow)
	row := func(special2 bool) *sfs.SFSArray {
		r := sfs.NewSFSObject()
		r.PutInt("level", 1)
		r.PutInt("normalState", 1)
		r.PutInt("specialState", 1)
		if special2 {
			r.PutInt("special2State", 0)
		}
		a := sfs.NewSFSArray()
		a.AddSFSObject(r)
		return a
	}
	for _, c := range []struct {
		name       string
		activity   int32
		highUnlock int32
		exchange   bool
		special2   bool
		want       bool
	}{
		{"high tier bought, row unclaimed", 19, 1, true, true, true},
		{"special2State missing", 19, 1, true, false, false},
		{"no high tier (no highExchangeId)", 19, 1, false, true, false},
		{"high tier not bought", 19, 0, true, true, false},
		{"login pass view", 13, 1, true, true, false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == battlePassInfoCmd {
				info := bpBoughtInfo(1, c.highUnlock)
				info.PutSFSArray("stateInfo", row(c.special2))
				if c.exchange {
					info.PutUtfString("highExchangeId", "8810021")
				}
				return info
			}
			return evOK()
		})
		if err := runBattlePass(conn, evInit(30, evActivity(c.activity))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(fake.only(battlePassAllCmd)) == 1; got != c.want {
			t.Errorf("%s: claim-all sent %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBattlePassScoreItemFollowsTheTablesPara5: the score item is the activity row's para_5, so a
// type-142 row the old hand-copied list never had (777142, para_5 620099) claims its premium-exp
// task, and a row without a numeric para_5 claims nothing (ActBattlePassInfo.lua:244-249).
func TestBattlePassScoreItemFollowsTheTablesPara5(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name      string
		para5     string
		wantClaim bool
	}{
		{"para_5 names the score item", "620099", true},
		{"no para_5", "", false},
		{"para_5 is no number", "620099|1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			const id = 777142
			withActivityRows(t, map[int32]activityRow{id: {typ: actTypeBattlePassNew, para5: c.para5}})
			if got := battlePassScoreItem(id) != 0; got != c.wantClaim {
				t.Errorf("battlePassScoreItem(%d) = %d", id, battlePassScoreItem(id))
			}
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == "hero.event.info.get" {
					info := bpBoughtInfo(1, 0, bpBoughtTask("t1", 2, -1, 620099, 3))
					info.PutInt("activityType", actTypeBattlePassNew)
					info.PutInt("activityId", id)
					return info
				}
				return evOK()
			})
			if err := runBattlePass(conn, evInit(30, evActivity(id))); err != nil {
				t.Fatal(err)
			}
			if got := len(fake.only(battlePassV2TaskCmd)) == 1; got != c.wantClaim {
				t.Errorf("bp.task.reward sent: %v, want %v", got, c.wantClaim)
			}
		})
	}
}

// eventTableDump reads table name from the JSON dump in LASTWAR_TABLES ({name, index:{col:[pos,
// type, linked]}, rows:[[key,[values]]]}, positions 1-based) as key -> column -> value. It skips the
// test when LASTWAR_TABLES is unset.
func eventTableDump(t *testing.T, name string) map[string]map[string]any {
	t.Helper()
	dir := os.Getenv("LASTWAR_TABLES")
	if dir == "" {
		t.Skip("LASTWAR_TABLES is not set (the directory of table JSON dumps, e.g. tables40413_json)")
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Index map[string][]any `json:"index"`
		Rows  [][]any          `json:"rows"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, r := range dump.Rows {
		if len(r) != 2 {
			continue
		}
		vals, _ := r[1].([]any)
		row := map[string]any{}
		for col, idx := range dump.Index {
			if len(idx) == 0 {
				continue
			}
			pos, _ := idx[0].(float64)
			if p := int(pos) - 1; p >= 0 && p < len(vals) {
				row[col] = vals[p]
			}
		}
		out[strings.TrimSuffix(strings.TrimSuffix(eventDumpScalar(r[0]), ".0"), " ")] = row
	}
	return out
}

func eventDumpScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
