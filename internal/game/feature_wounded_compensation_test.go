package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

type woundedEntry struct {
	uuid int64
	at   time.Time // zero: no time field
}

func woundedInit(entries ...woundedEntry) *Init {
	return rewardInit(func(raw *sfs.SFSObject) {
		arr := sfs.NewSFSArray()
		for _, e := range entries {
			o := sfs.NewSFSObject()
			o.PutLong("uuid", e.uuid)
			if !e.at.IsZero() {
				o.PutLong("time", e.at.UnixMilli())
			}
			o.PutDouble("scale", 0.3)
			arr.AddSFSObject(o)
		}
		raw.PutSFSArray("compensateArr", arr)
	})
}

func woundedSentUUIDs(fake *rewardFake) []int64 {
	var out []int64
	for _, r := range fake.requests() {
		if r.Cmd == woundedCompensationCmd {
			out = append(out, r.Params.GetLong("uuid"))
		}
	}
	return out
}

func TestWoundedCompensationClaimsOnlyAfterTheWait(t *testing.T) {
	now := time.Now()
	in := woundedInit(
		woundedEntry{11, now.Add(-2 * time.Hour)},    // claimable (wait is 1 h)
		woundedEntry{12, now.Add(-30 * time.Minute)}, // still "Collectable in ..."
		woundedEntry{13, time.Time{}},                // no time: skipped
		woundedEntry{14, now.Add(-25 * time.Hour)},   // claimable
	)
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runWoundedCompensation(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := woundedSentUUIDs(fake); !slices.Equal(got, []int64{11, 14}) {
		t.Errorf("claimed %v, want [11 14]", got)
	}
	if n := len(fake.cmds()); n != 2 {
		t.Errorf("sent %d requests, want 2", n)
	}
}

func TestWoundedCompensationNothingDueSendsNothing(t *testing.T) {
	for name, in := range map[string]*Init{
		"no init":  {},
		"none":     rewardInit(func(*sfs.SFSObject) {}),
		"too soon": woundedInit(woundedEntry{1, time.Now().Add(-time.Minute)}),
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
			if err := runWoundedCompensation(conn, in); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
		})
	}
}

func TestWoundedCompensationBenignFailureAndDeadConnection(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	in := woundedInit(woundedEntry{1, old}, woundedEntry{2, old}, woundedEntry{3, old}, woundedEntry{4, old})
	replies := map[int64]*sfs.SFSObject{1: rewardErr("120289"), 2: rewardErr("999999"), 3: nil}
	conn, fake := startRewardFake(t, func(_ string, p *sfs.SFSObject) *sfs.SFSObject {
		return replies[p.GetLong("uuid")]
	})
	err := runWoundedCompensation(conn, in)
	if err == nil {
		t.Fatal("run = nil, want the failure on uuid 2 and the dead connection on uuid 3")
	}
	// 1 is benign, 2 fails but the loop goes on, 3 drops the connection so 4 is never sent.
	if got := woundedSentUUIDs(fake); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("claimed %v, want [1 2 3]", got)
	}
}
