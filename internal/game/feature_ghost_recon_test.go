package game

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// ghostTask is one taskList entry with ActGhostreconTaskInfo's field names.
type ghostTask struct {
	uuid           int64
	owner          string
	teamStart      int64
	completion     time.Time // zero: not running yet
	members        map[string]int32
	rewardedAbsent bool // own member entry without a rewarded field
}

func ghostTaskList(tasks ...ghostTask) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutLong("openTime", evTestNow.Add(-time.Hour).UnixMilli())
	r.PutLong("dispatchBeginTime", evTestNow.Add(-time.Hour).UnixMilli())
	r.PutLong("dispatchEndTime", evTestNow.Add(time.Hour).UnixMilli())
	a := sfs.NewSFSArray()
	for _, t := range tasks {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", t.uuid)
		o.PutUtfString("ownerId", t.owner)
		o.PutUtfString("allianceId", "fake-alliance")
		o.PutInt("ownerServer", 321)
		o.PutLong("teamStartTime", t.teamStart)
		if !t.completion.IsZero() {
			o.PutLong("completionTime", t.completion.UnixMilli())
		} else {
			o.PutLong("completionTime", 0)
		}
		ms := sfs.NewSFSArray()
		for _, uid := range slices.Sorted(maps.Keys(t.members)) {
			m := sfs.NewSFSObject()
			m.PutUtfString("uid", uid)
			if !t.rewardedAbsent || uid != "me" {
				m.PutInt("rewarded", t.members[uid])
			}
			ms.AddSFSObject(m)
		}
		o.PutSFSArray("memberList", ms)
		a.AddSFSObject(o)
	}
	r.PutSFSArray("taskList", a)
	return r
}

// ghostInit is an alliance Init at HQ hq with Ghost Recon 94110 running, own uid "me" and
// serverId 321, edited by user.
func ghostInit(hq int32, user func(u *sfs.SFSObject)) *Init {
	in := evInit(hq, evActivity(94110))
	u := sfs.NewSFSObject()
	u.PutUtfString("uid", "me")
	u.PutUtfString("allianceId", "fake-alliance")
	u.PutInt("serverId", 321)
	if user != nil {
		user(u)
	}
	in.Raw.PutSFSObject("user", u)
	return in
}

func runGhostFake(t *testing.T, in *Init, list, batch *sfs.SFSObject) (*evFake, error) {
	t.Helper()
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case ghostReconTaskListCmd:
			return list
		case ghostReconBatchCmd:
			if batch != nil {
				return batch
			}
		}
		return evOK()
	})
	err := runGhostRecon(conn, in)
	for _, c := range fake.cmds() {
		if c != ghostReconTaskListCmd && c != ghostReconBatchCmd { // never alliance list, steal, join, start, kick
			t.Errorf("sent %s; the feature only reads the task list and claims", c)
		}
	}
	return fake, err
}

func TestGhostReconBatchClaimsWaitingTasks(t *testing.T) {
	past, future := evTestNow.Add(-time.Minute), evTestNow.Add(time.Hour)
	start := evTestNow.Add(-2 * time.Hour).UnixMilli()
	list := ghostTaskList(
		ghostTask{uuid: 1, owner: "me", teamStart: start, completion: past, members: map[string]int32{"me": 0, "ally": 0}}, // Own_WaitClaim
		ghostTask{uuid: 2, owner: "me", teamStart: start, completion: past, members: map[string]int32{"me": 1}},            // Own_Claimed
		ghostTask{uuid: 3, owner: "me", teamStart: start, completion: future, members: map[string]int32{"me": 0}},          // Own_Runing
		ghostTask{uuid: 4, owner: "me", teamStart: 0, completion: past, members: map[string]int32{"me": 0}},                // Own_NotExecuted
		ghostTask{uuid: 5, owner: "ally", completion: past, members: map[string]int32{"ally": 0, "me": 0}},                 // Alliance_WaitClaim (joined)
		ghostTask{uuid: 6, owner: "ally", completion: past, members: map[string]int32{"ally": 0}},                          // not joined: OwnIsRewarded
		ghostTask{uuid: 7, owner: "ally", members: map[string]int32{"ally": 0, "me": 0}},                                   // Alliance_TeamingUp
		ghostTask{uuid: 9007199254740993, owner: "ally", completion: past, members: map[string]int32{"me": 0}, rewardedAbsent: true},
	)
	fake, err := runGhostFake(t, ghostInit(30, nil), list, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{ghostReconTaskListCmd, ghostReconBatchCmd}) {
		t.Fatalf("sent %v", got)
	}
	p := fake.only(ghostReconBatchCmd)[0].Params
	v, _ := p.Get("uuidList")
	if v.Type != sfsLongArrayTag {
		t.Errorf("uuidList type %d, want LongArray", v.Type)
	}
	if got, _ := v.Val.([]int64); !slices.Equal(got, []int64{1, 5, 9007199254740993}) {
		t.Errorf("uuidList = %v, want [1 5 9007199254740993]", v.Val)
	}
	if s, _ := p.Get("ownerServer"); s.Type != sfs.SFSInt || p.GetInt("ownerServer") != 321 {
		t.Errorf("ownerServer = %#v, want Int 321", s)
	}
}

func TestGhostReconOwnerServerIsTheSourceServer(t *testing.T) {
	list := ghostTaskList(ghostTask{uuid: 1, owner: "me", teamStart: 1, completion: evTestNow.Add(-time.Minute), members: map[string]int32{"me": 0}})
	for _, c := range []struct {
		name string
		user func(u *sfs.SFSObject)
		want int32 // 0: nothing sent
	}{
		{"home", nil, 321},
		{"cross-server fight", func(u *sfs.SFSObject) { u.PutInt("crossFightSrcServerId", 456) }, 456},
		{"crossFightSrcServerId unset", func(u *sfs.SFSObject) { u.PutInt("crossFightSrcServerId", -1) }, 321},
		{"crossFightSrcServerId 0, as the client reads it", func(u *sfs.SFSObject) { u.PutInt("crossFightSrcServerId", 0) }, 0},
		{"no serverId", func(u *sfs.SFSObject) { u.PutInt("serverId", 0) }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake, err := runGhostFake(t, ghostInit(30, c.user), list, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := fake.only(ghostReconBatchCmd)
			switch {
			case c.want == 0 && len(fake.cmds()) != 0:
				t.Errorf("sent %v, want nothing without a source server", fake.cmds())
			case c.want != 0 && (len(got) != 1 || got[0].Params.GetInt("ownerServer") != c.want):
				t.Errorf("sent %v, want one batch with ownerServer %d", fake.cmds(), c.want)
			}
		})
	}
}

func TestGhostReconGates(t *testing.T) {
	waiting := ghostTaskList(ghostTask{uuid: 1, owner: "me", teamStart: 1, completion: evTestNow.Add(-time.Minute), members: map[string]int32{"me": 0}})
	noOpen := withoutKey(waiting, "openTime")
	nothing := ghostTaskList(ghostTask{uuid: 1, owner: "me", teamStart: 1, completion: evTestNow.Add(time.Minute), members: map[string]int32{"me": 0}})
	for _, c := range []struct {
		name string
		in   *Init
		list *sfs.SFSObject
		want []string
	}{
		{"HQ below the table's 18", ghostInit(17, nil), waiting, nil},
		{"no Ghost Recon activity", func() *Init {
			in := ghostInit(30, nil)
			in.Raw.PutSFSObject("activity", sfs.NewSFSObject())
			return in
		}(), waiting, nil},
		{"no own uid", ghostInit(30, func(u *sfs.SFSObject) { u.PutUtfString("uid", "") }), waiting, nil},
		{"reply without openTime", ghostInit(30, nil), noOpen, []string{ghostReconTaskListCmd}},
		{"nothing waiting", ghostInit(30, nil), nothing, []string{ghostReconTaskListCmd}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake, err := runGhostFake(t, c.in, c.list, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestGhostReconBatchErrorIsReturned(t *testing.T) {
	list := ghostTaskList(ghostTask{uuid: 1, owner: "me", teamStart: 1, completion: evTestNow.Add(-time.Minute), members: map[string]int32{"me": 0}})
	_, err := runGhostFake(t, ghostInit(30, nil), list, evErr(evAlreadyExecuted))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s reply reported (no benign codes for this feature)", err, evAlreadyExecuted)
	}
}

func TestGhostReconIsOptIn(t *testing.T) {
	f, ok := featureRegistry[ghostReconFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, DefaultOn false, no duel scores, static-only summary", f)
	}
}
