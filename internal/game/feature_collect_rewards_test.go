package game

import (
	"maps"
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func collectRewardsInit(entries map[int64]time.Time) *Init {
	return rewardInit(func(raw *sfs.SFSObject) {
		arr := sfs.NewSFSArray()
		for _, uuid := range slices.Sorted(maps.Keys(entries)) {
			e := sfs.NewSFSObject()
			e.PutLong("uuid", uuid)
			e.PutLong("expireTime", entries[uuid].UnixMilli())
			e.PutInt("type", 2)
			arr.AddSFSObject(e)
		}
		raw.PutSFSArray("collect_reward", arr)
	})
}

// collectRewardsSent returns the uuidArr of each gather.collect.reward request, checking that it
// is an SFSArray of Longs as GatherCollectRewardMessage builds it.
func collectRewardsSent(t *testing.T, fake *rewardFake) [][]int64 {
	t.Helper()
	var out [][]int64
	for _, r := range fake.requests() {
		if r.Cmd != collectRewardsCmd {
			t.Errorf("unexpected cmd %q", r.Cmd)
			continue
		}
		v, ok := r.Params.Get("uuidArr")
		arr, isArr := v.Val.(*sfs.SFSArray)
		if !ok || !isArr {
			t.Fatalf("uuidArr missing or not an SFSArray: %#v", v)
		}
		var uuids []int64
		for _, it := range arr.Items() {
			if it.Type != sfs.SFSLong {
				t.Errorf("uuidArr item type %d, want Long", it.Type)
			}
			uuids = append(uuids, it.Val.(int64))
		}
		out = append(out, uuids)
	}
	return out
}

func TestCollectRewardsClaimsOnlyUnexpired(t *testing.T) {
	now := time.Now()
	in := collectRewardsInit(map[int64]time.Time{
		101: now.Add(time.Hour),
		102: now.Add(-time.Hour),      // expired (in ms; compared against seconds it would pass)
		103: now.Add(2 * time.Second), // inside the expiry margin
		104: now.Add(48 * time.Hour),
		0:   now.Add(time.Hour),       // no usable uuid
		105: now.Add(-48 * time.Hour), // expired
		106: now.Add(30 * time.Minute),
	})
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runCollectRewards(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := collectRewardsSent(t, fake)
	if len(got) != 1 || !slices.Equal(got[0], []int64{101, 104, 106}) {
		t.Errorf("sent %v, want one claim of [101 104 106]", got)
	}
}

func TestCollectRewardsNothingDueSendsNothing(t *testing.T) {
	for name, in := range map[string]*Init{
		"no init":     {},
		"empty list":  rewardInit(func(*sfs.SFSObject) {}),
		"all expired": collectRewardsInit(map[int64]time.Time{7: time.Now().Add(-time.Minute)}),
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
			if err := runCollectRewards(conn, in); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
		})
	}
}

func TestCollectRewardsAlreadyClaimedIsBenign(t *testing.T) {
	conn, _ := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardErr("120289") })
	if err := runCollectRewards(conn, collectRewardsInit(map[int64]time.Time{1: time.Now().Add(time.Hour)})); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
}

func TestCollectRewardsBatchesAndStopsOnFailure(t *testing.T) {
	entries := map[int64]time.Time{}
	for i := int64(1); i <= 450; i++ {
		entries[i] = time.Now().Add(time.Hour)
	}
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runCollectRewards(conn, collectRewardsInit(entries)); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := collectRewardsSent(t, fake)
	if len(got) != 3 || len(got[0]) != 200 || len(got[1]) != 200 || len(got[2]) != 50 {
		t.Errorf("batch sizes wrong: %d batches", len(got))
	}

	conn, fake = startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardErr("999999") })
	if err := runCollectRewards(conn, collectRewardsInit(entries)); err == nil {
		t.Error("run = nil, want the claim failure")
	}
	if n := len(fake.cmds()); n != 1 {
		t.Errorf("sent %d batches after a failure, want 1", n)
	}
}
