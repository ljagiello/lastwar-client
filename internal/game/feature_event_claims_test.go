package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func eventScoreBoxes(score int64, boxes ...[3]int32) *sfs.SFSObject { // needScore, rewardFlag, vipRewardFlag
	info := sfs.NewSFSObject()
	info.PutLong("score", score)
	l := sfs.NewSFSArray()
	for _, bx := range boxes {
		o := sfs.NewSFSObject()
		o.PutInt("needScore", bx[0])
		o.PutInt("rewardFlag", bx[1])
		o.PutInt("vipRewardFlag", bx[2])
		o.PutInt("needVipLevel", 10)
		l.AddSFSObject(o)
	}
	info.PutSFSArray("scoreReward", l)
	return info
}

func eventTasks(key string, tasks map[string]int32) *sfs.SFSArray {
	l := sfs.NewSFSArray()
	for id, st := range tasks {
		o := sfs.NewSFSObject()
		o.PutUtfString(key, id)
		o.PutInt("state", st)
		l.AddSFSObject(o)
	}
	return l
}

func TestEventClaimsActivities(t *testing.T) {
	withEvNow(t, evTestNow)
	v2Infos, actInfos := 0, 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case redBossProgressCmd:
			r.PutInt("claimableCount", 2)
		case redBossAchievementCmd:
			r.PutSFSArray("tasks", eventTasks("id", map[string]int32{"a": 2}))
		case redBossMarchCmd:
			r.PutInt("dailyRewardTarget", 5)
			r.PutInt("dailyAtkCount", 5)
			r.PutInt("dailyRewardClaimed", 0)
		case winterStormInfoCmd:
			r.PutLong("score", 100000)
			d := sfs.NewSFSArray()
			d.AddInt(201)
			r.PutSFSArray("data", d)
		case sevenDayInfoCmd:
			return eventScoreBoxes(500, [3]int32{100, 1, 1}, [3]int32{300, 0, 0}, [3]int32{900, 0, 0})
		case sevenDayV2InfoCmd:
			v2Infos++
			score := int64(50)
			if v2Infos > 1 {
				score = 150 // the task claim raised the score
			}
			r = eventScoreBoxes(score, [3]int32{100, 0, 1})
			// three entries per day: day 1 = 0-2, day 2 = 3-5, day 3 = 6; the event is on day 2
			acts := sfs.NewSFSArray()
			for i := range 7 {
				d := sfs.NewSFSObject()
				tasks := map[string]int32{}
				switch i {
				case 0:
					tasks["11"] = 1
				case 4:
					tasks["22"] = 1
				case 5:
					tasks["23"] = 2
				case 6:
					tasks["33"] = 1 // day 3: locked
				}
				d.PutSFSArray("tasks", eventTasks("id", tasks))
				acts.AddSFSObject(d)
			}
			r.PutSFSArray("dayActs", acts)
		case bountyHunterInfoCmd:
			sk := sfs.NewSFSObject()
			sk.PutSFSArray("stashReward", freebieReward())
			r.PutSFSObject("stageKey", sk)
		case "hero.event.info.get":
			actInfos++
			r.PutInt("activityType", actTypeActTask)
			if actInfos == 1 {
				r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"t1": 1}))
				r.PutLong("achieve_score", 10)
			} else {
				r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"t1": 2}))
				r.PutLong("achieve_score", 250)
			}
			ach := sfs.NewSFSArray()
			for _, s := range []int32{100, 200, 300} {
				o := sfs.NewSFSObject()
				o.PutInt("targetScore", s)
				ach.AddSFSObject(o)
			}
			r.PutSFSArray("achieveArr", ach)
			rec := sfs.NewSFSArray()
			rec.AddInt(0) // milestone 0 already claimed
			r.PutSFSArray("score_receives", rec)
		case arenaInfoCmd: // no arena open
		default:
			return evOK()
		}
		return r
	})
	v2 := evActivity(95101)
	v2.PutLong("startTime", evTestNow.Add(-30*time.Hour).UnixMilli()) // day 2 of the event
	in := evInit(30, evActivity(80152), evActivity(40039), evActivity(3013), evActivity(95003), v2,
		evActivity(98800), evActivity(99010))
	if err := runEventClaims(conn, in); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{
		"red.boss.get.progress", "red.boss.claim.all", "red.boss.get.achievement.info", "red.boss.get.march", "red.boss.claim.daily.reward",
		"winter.storm.reward.info", "winter.storm.reward.get.all",
		"get.seven.day.act.info", "receive.seven.day.act.reward", "get.seven.day.act.info", "receive.seven.day.act.all.reward",
		"seven.day.v2.info", "seven.day.v2.receive.task", "seven.day.v2.receive.task", "seven.day.v2.info", "seven.day.v2.receive.score",
		"bounty.hunter.get.info", "bounty.hunter.receive.drop.reward",
		"hero.event.info.get", "activity.task.reward", "hero.event.info.get", "activity.score.reward",
		"arena.info",
	}, ",")
	if got != want {
		t.Fatalf("sent\n%s\nwant\n%s", got, want)
	}
	if p := fake.only(redBossClaimAllCmd)[0].Params; p.GetInt("type") != 1 {
		t.Errorf("crystal boss claim type = %d, want 1 (no achievement task is claimable)", p.GetInt("type"))
	}
	if p := fake.only(sevenDayOneCmd)[0].Params; p.GetInt("activityId") != 3013 || p.GetInt("index") != 2 {
		t.Errorf("seven-day per-box claim = %v, want {activityId:3013, index:2}", p)
	}
	tasks := fake.only(sevenDayV2TaskCmd)
	if len(tasks) != 2 || tasks[0].Params.GetString("taskId") != "11" || tasks[1].Params.GetString("taskId") != "22" ||
		tasks[0].Params.GetInt("activityId") != 95101 {
		t.Errorf("v2 task claims = %v, want 11 and 22 (33 is on locked day 3)", tasks)
	}
	if p := fake.only(actScoreRewardCmd)[0].Params; p.GetInt("index") != 1 {
		t.Errorf("milestone index = %d, want 1 (0 claimed, 2 not reached)", p.GetInt("index"))
	}
	if p := fake.only(actTaskRewardCmd)[0].Params; p.GetString("taskId") != "t1" || p.GetInt("activityId") != 99010 {
		t.Errorf("activity task claim = %v", p)
	}
}

func TestEventClaimsArenaAndFlowerTrain(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case arenaInfoCmd:
			open := sfs.NewSFSObject()
			open.PutLong("showTime", 1)
			open.PutLong("startTime", evTestNow.Add(-time.Hour).UnixMilli())
			open.PutLong("endTime", evTestNow.Add(time.Hour).UnixMilli())
			r.PutSFSObject("newArenaInfo", open)
			closed := sfs.NewSFSObject()
			closed.PutLong("showTime", 0)
			r.PutSFSObject("galeArenaInfo", closed)
		case "new.arena.rank.list":
			data := sfs.NewSFSObject()
			data.PutInt("battleCount", 5)
			boxes := sfs.NewSFSArray()
			for _, bx := range [][2]int32{{1, 1}, {3, 0}, {10, 0}} {
				o := sfs.NewSFSObject()
				o.PutInt("needCount", bx[0])
				o.PutInt("rewarded", bx[1])
				boxes.AddSFSObject(o)
			}
			data.PutSFSArray("dailyReward", boxes)
			r.PutSFSObject("rankData", data)
		default:
			return evOK()
		}
		return r
	})
	in := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("uid", "me")
	in.Raw.PutSFSObject("user", u)
	trains := sfs.NewSFSArray()
	for _, tr := range []struct {
		uuid    int64
		uid     string
		arrived int32
		at      time.Time
	}{{71, "me", 0, evTestNow.Add(-time.Minute)}, {72, "me", 0, evTestNow.Add(time.Hour)}, {73, "other", 1, evTestNow}, {74, "me", 1, evTestNow.Add(time.Hour)}} {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", tr.uuid)
		o.PutUtfString("uid", tr.uid)
		o.PutInt("arrived", tr.arrived)
		o.PutLong("arriveTime", tr.at.UnixMilli())
		trains.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("flowerTrainArr", trains)
	if err := runEventClaims(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "arena.info,new.arena.rank.list,new.arena.reward,flower.train.receive.reward,flower.train.receive.reward" {
		t.Fatalf("sent %s", got)
	}
	if p := fake.only("new.arena.reward")[0].Params; p.GetInt("target") != 2 {
		t.Errorf("arena box = %d, want 2 (1-based; box 1 rewarded, box 3 not reached)", p.GetInt("target"))
	}
	trainsSent := fake.only(flowerTrainCmd)
	if trainsSent[0].Params.GetLong("trainUuid") != 71 || trainsSent[1].Params.GetLong("trainUuid") != 74 {
		t.Errorf("trains = %v, %v; want 71 and 74", trainsSent[0].Params, trainsSent[1].Params)
	}
}

func TestEventClaimsCrystalBossSilentErrorIsBenign(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case redBossProgressCmd:
			r.PutInt("claimableCount", 1)
		case redBossClaimAllCmd:
			return evErr(redBossSilentError)
		}
		return r
	})
	if err := runEventClaims(conn, evInit(30, evActivity(80152))); err != nil {
		t.Errorf("E100172 on red.boss.claim.all must be benign, got %v", err)
	}
}

func TestEventClaimsBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case bountyHunterInfoCmd:
			r := sfs.NewSFSObject()
			sk := sfs.NewSFSObject()
			sk.PutSFSArray("stashReward", freebieReward())
			r.PutSFSObject("stageKey", sk)
			return r
		case bountyHunterDropCmd:
			if p.GetInt("activityId") == 98800 {
				return evErr(evAlreadyExecuted)
			}
			return evErr("E6")
		}
		return evOK()
	})
	err := runEventClaims(conn, evInit(30, evActivity(98800), evActivity(98802)))
	if err == nil || !strings.Contains(err.Error(), "E6") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E6 failure", err)
	}
}
