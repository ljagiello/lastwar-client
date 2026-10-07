package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

type radarEvent struct {
	uuid    int64
	eventID int32
	state   int32
}

// radarInfo builds a get.detect.info reply; asMap sends events as an object keyed by uuid.
func radarInfo(level, rewardLevel int32, asMap bool, events ...radarEvent) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	d := sfs.NewSFSObject()
	d.PutInt("level", level)
	if rewardLevel > 0 {
		d.PutInt("rewardLevel", rewardLevel)
	}
	resp.PutSFSObject("detectInfo", d)
	arr := sfs.NewSFSArray()
	m := sfs.NewSFSObject()
	for i, e := range events {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", e.uuid)
		o.PutInt("eventId", e.eventID)
		o.PutInt("state", e.state)
		arr.AddSFSObject(o)
		m.PutSFSObject(string(rune('a'+i)), o)
	}
	if asMap {
		resp.PutSFSObject("events", m)
	} else {
		resp.PutSFSArray("events", arr)
	}
	return resp
}

// radarRun answers the level claim by advancing rewardLevel by one, up to stopAt.
func radarRun(t *testing.T, info *sfs.SFSObject, eventReply *sfs.SFSObject, stopAt int32) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case radarInfoCmd:
			return info
		case radarEventCmd:
			return eventReply
		}
		resp := rewardOK()
		next := min(p.GetInt("rewardLevel")+1, stopAt)
		resp.PutInt("rewardLevel", next)
		return resp
	})
	return fake, runRadarClaims(conn, rewardInitWithDay(20, func(*sfs.SFSObject) {}))
}

func TestRadarClaimsFinishedEventsAndLevels(t *testing.T) {
	for _, asMap := range []bool{false, true} {
		info := radarInfo(4, 2, asMap,
			radarEvent{11, 1004, 1},   // finished: claim
			radarEvent{12, 1005, 0},   // running
			radarEvent{13, 1006, 2},   // already rewarded
			radarEvent{14, 410002, 1}, // cockatrice guide: skipped
			radarEvent{15, 310050, 1}, // rescue: skipped
			radarEvent{16, 21001, 1},  // claim
		)
		fake, err := radarRun(t, info, rewardOK(), 4)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var events []int64
		var levels []int32
		for _, r := range fake.requests() {
			switch r.Cmd {
			case radarEventCmd:
				events = append(events, r.Params.GetLong("uuid"))
			case radarLevelCmd:
				levels = append(levels, r.Params.GetInt("rewardLevel"))
			case radarInfoCmd:
				if v, _ := r.Params.Get("openWnd"); v.Val != false {
					t.Errorf("openWnd = %v, want false", v.Val)
				}
			}
		}
		slices.Sort(events)
		if !slices.Equal(events, []int64{11, 16}) {
			t.Errorf("asMap=%v: claimed events %v, want [11 16]", asMap, events)
		}
		if !slices.Equal(levels, []int32{2, 3}) {
			t.Errorf("asMap=%v: claimed levels %v, want [2 3] (rewardLevel sent as is, until it reaches level 4)", asMap, levels)
		}
	}
}

func TestRadarClaimsNothingDue(t *testing.T) {
	fake, err := radarRun(t, radarInfo(3, 3, false, radarEvent{1, 1004, 0}), rewardOK(), 3)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{radarInfoCmd}) {
		t.Errorf("sent %v, want only the info read", got)
	}
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runRadarClaims(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v; want nothing", err, fake.cmds())
	}
}

func TestRadarClaimsLevelLoopStopsWhenStuck(t *testing.T) {
	// The reply never advances rewardLevel: one claim, then stop.
	fake, err := radarRun(t, radarInfo(9, 0, false), rewardOK(), 1)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var levels []int32
	for _, r := range fake.requests() {
		if r.Cmd == radarLevelCmd {
			levels = append(levels, r.Params.GetInt("rewardLevel"))
		}
	}
	if !slices.Equal(levels, []int32{1}) {
		t.Errorf("claimed levels %v, want [1] (rewardLevel defaults to 1, then the loop stops)", levels)
	}
}

func TestRadarClaimsBenignAndFailure(t *testing.T) {
	info := radarInfo(1, 1, false, radarEvent{1, 1004, 1}, radarEvent{2, 1004, 1})
	if _, err := radarRun(t, info, rewardErr("120289"), 1); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	fake, err := radarRun(t, info, rewardErr("999999"), 1)
	if err == nil {
		t.Error("run = nil, want the claim failures")
	}
	if n := len(fake.cmds()); n != 3 {
		t.Errorf("sent %d requests, want info + both claims", n)
	}
}
