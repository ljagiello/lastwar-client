package game

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func bountyInfo(score int32, claimed []int32, chests ...string) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	ck := sfs.NewSFSObject()
	ck.PutInt("score", score)
	pr := sfs.NewSFSArray()
	for _, c := range claimed {
		pr.AddInt(c)
	}
	ck.PutSFSArray("phaseReward", pr)
	r.PutSFSObject("commonKey", ck)
	sk := sfs.NewSFSObject()
	ca := sfs.NewSFSArray()
	for _, u := range chests {
		o := sfs.NewSFSObject()
		n, _ := strconv.ParseInt(u, 10, 64)
		o.PutLong("uuid", n)
		o.PutInt("id", 1)
		ca.AddSFSObject(o)
	}
	sk.PutSFSArray("chestArray", ca)
	r.PutSFSObject("stageKey", sk)
	return r
}

// G25: thresholds the score reached and not in commonKey.phaseReward send
// bounty.hunter.receive.phase.reward {activityId: Int, phaseId: Int <threshold>}
// (BountyHunterPhaseItemComponent.lua:64-103; BountyHunterReceivePhaseRewardMessage.lua:6-7).
func TestBountyMilestones(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == bountyHunterInfoCmd {
			return bountyInfo(2000, []int32{300})
		}
		return evOK()
	})
	if err := runBountyMilestones(conn, evInit(30, evActivity(98800))); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.only(bountyPhaseRewardCmd) {
		v, _ := r.Params.Get("phaseId")
		if v.Type != sfs.SFSInt || r.Params.GetInt("activityId") != 98800 {
			t.Errorf("claim %v: phaseId must be Int, activityId 98800", r.Params)
		}
		got = append(got, strconv.Itoa(int(r.Params.GetInt("phaseId"))))
	}
	if strings.Join(got, ",") != "1000,1800" {
		t.Errorf("milestones claimed %v, want 1000 and 1800 (300 claimed, 3000 not reached)", got)
	}
}

// G25: an activity whose tableInfoType names no activity_hunter row has no milestones, and a claim
// error stops the feature (no benign code).
func TestBountyMilestonesRowAndErrors(t *testing.T) {
	withEvNow(t, evTestNow)
	saved := activityTable[98800]
	row := saved
	row.subType = 99
	activityTable[98800] = row
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return bountyInfo(9000, nil) })
	err := runBountyMilestones(conn, evInit(30, evActivity(98800)))
	activityTable[98800] = saved
	if err != nil || len(fake.requests()) != 0 {
		t.Errorf("unknown row: err %v, sent %v; want nothing", err, fake.cmds())
	}

	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == bountyHunterInfoCmd {
			return bountyInfo(9000, nil)
		}
		return evErr(evAlreadyExecuted)
	})
	err = runBountyMilestones(conn2, evInit(30, evActivity(98800)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake2.only(bountyPhaseRewardCmd)); n != 1 {
		t.Errorf("claims = %d, want 1", n)
	}
}

// G25: each scene chest sends bounty.hunter.click.chest.reward {activityId: Int, targetId:
// UtfString uuid} (BountyHunterClickChestRewardMessage.lua:4-8), then a non-empty stash is claimed.
func TestBountyChests(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case bountyHunterInfoCmd:
			return bountyInfo(0, nil, "8800000000001", "8800000000002")
		case bountyClickChestCmd:
			r := sfs.NewSFSObject()
			r.PutInt("activityId", 98800)
			r.PutUtfString("targetId", p.GetString("targetId"))
			r.PutSFSArray("stashReward", freebieReward())
			return r
		}
		return evOK()
	})
	if err := runBountyChests(conn, evInit(30, evActivity(98800))); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{bountyHunterInfoCmd, bountyClickChestCmd, bountyClickChestCmd, bountyHunterDropCmd}, ",")
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	c := fake.only(bountyClickChestCmd)[1].Params
	if v, _ := c.Get("targetId"); v.Type != sfs.SFSUtfString || v.Val != "8800000000002" {
		t.Errorf("targetId %#v, want UtfString 8800000000002", v)
	}
	if v, _ := c.Get("activityId"); v.Type != sfs.SFSInt {
		t.Errorf("activityId %#v, want Int", v)
	}
}

// G25: no chests sends nothing past the read; a click error stops the feature before the stash.
func TestBountyChestsNoneAndErrors(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return bountyInfo(0, nil) })
	if err := runBountyChests(conn, evInit(30, evActivity(98800))); err != nil || strings.Join(fake.cmds(), ",") != bountyHunterInfoCmd {
		t.Errorf("no chests: err %v, sent %v", err, fake.cmds())
	}
	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == bountyHunterInfoCmd {
			return bountyInfo(0, nil, "1", "2")
		}
		return evErr(evAlreadyExecuted)
	})
	err := runBountyChests(conn2, evInit(30, evActivity(98800)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if got := strings.Join(fake2.cmds(), ","); got != bountyHunterInfoCmd+","+bountyClickChestCmd {
		t.Errorf("sent %s, want the read and one click", got)
	}
}

// TestBountyHunterPhaseScoresMatchTheDump pins bountyPhaseScores and bountyHunterRows to table
// activity_hunter: every row's score_reward thresholds, with a score_reward_show entry for each.
func TestBountyHunterPhaseScoresMatchTheDump(t *testing.T) {
	rows := eventTableDump(t, "activity_hunter")
	if len(rows) != bountyHunterRows {
		t.Errorf("activity_hunter has %d rows, bountyHunterRows is %d", len(rows), bountyHunterRows)
	}
	for key, r := range rows {
		var got []int64
		for _, part := range strings.Split(eventDumpScalar(r["score_reward"]), "|") {
			f := strings.Split(part, ";")
			if len(f) != 2 {
				continue
			}
			n, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				t.Fatalf("row %s score_reward %q", key, r["score_reward"])
			}
			got = append(got, n)
		}
		if !slices.Equal(got, bountyPhaseScores) {
			t.Errorf("row %s thresholds %v, want %v", key, got, bountyPhaseScores)
		}
		if shows := strings.Split(eventDumpScalar(r["score_reward_show"]), "|"); len(shows) < len(got) {
			t.Errorf("row %s has %d score_reward_show entries for %d thresholds", key, len(shows), len(got))
		}
	}
}
