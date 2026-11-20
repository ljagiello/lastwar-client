package game

import (
	"maps"
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

// WP-Z touch point 5 (MASTER B64 and B61).

// TestSeasonPreTasksSendTheEntryID: the season preview sends its entry's own id (data.id,
// SeasonPreviewManager.lua:22-24, 91), never the entry's activityid, in both the task view and the
// claim.
func TestSeasonPreTasksSendTheEntryID(t *testing.T) {
	withEvNow(t, evTestNow)
	var preview int32
	for _, id := range slices.Sorted(maps.Keys(activityTable)) {
		if r := activityTable[id]; r.typ == actTypeSeasonPreview && r.needHQ <= 30 {
			preview = id
			break
		}
	}
	if preview == 0 {
		t.Skip("no season preview row in the activity table")
	}
	entry := evActivity(preview)
	entry.PutUtfString("activityid", "70000")
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonPreTaskInfoCmd {
			r := sfs.NewSFSObject()
			tasks := sfs.NewSFSArray()
			task := sfs.NewSFSObject()
			task.PutUtfString("taskId", "t1")
			task.PutInt("state", seasonPreTaskCanReceive)
			tasks.AddSFSObject(task)
			r.PutSFSArray("seasonPreTaskArr", tasks)
			return r
		}
		return evOK()
	})
	if err := runSeasonDailies(conn, evInit(30, entry)); err != nil {
		t.Fatal(err)
	}
	want := (Activity{ID: preview}).EntryID()
	for _, cmd := range []string{seasonPreTaskInfoCmd, seasonPreTaskRewardCmd} {
		reqs := fake.only(cmd)
		if len(reqs) != 1 {
			t.Fatalf("%s sent %d times, want 1 (sent %v)", cmd, len(reqs), fake.cmds())
		}
		if got := reqs[0].Params.GetString("activityId"); got != want {
			t.Errorf("%s activityId = %q, want the entry's id %q", cmd, got, want)
		}
	}
}

// TestTorchRelayFreebieNeedsItsTableRow: the Torch Relay free pack needs the activity's
// activity_torch_relay row with free_iap_reward > 0 (CanClaimFreePackage,
// ActivityTorchRelayData.lua:220-224; every row of tables 40413 has one), so a type-309 activity
// whose row is unknown is neither read nor claimed, as claimTorchRelay skips it.
func TestTorchRelayFreebieNeedsItsTableRow(t *testing.T) {
	withEvNow(t, evTestNow)
	withActivityRows(t, map[int32]activityRow{777309: {typ: actTypeTorchRelay, subType: 9}})
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			r := sfs.NewSFSObject()
			r.PutInt("activityType", actTypeTorchRelay)
			return r
		}
		return evOK()
	})
	if err := runEventShopFreebies(conn, evInit(30, evActivity(777309), evActivity(99162))); err != nil {
		t.Fatal(err)
	}
	claims := fake.only("torch.relay.daily.reward")
	if len(claims) != 1 || claims[0].Params.GetInt("activityId") != 99162 {
		t.Errorf("torch.relay.daily.reward sent %v, want once, for 99162 only", claims)
	}
	for _, r := range fake.only("hero.event.info.get") {
		if r.Params.GetString("activityId") == "777309" {
			t.Error("read the info of a Torch Relay activity without a table row")
		}
	}
}
