package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

// allianceInit is an init inside an alliance (alliance.uid set) with the given salary fields.
func allianceInit(fill func(raw *sfs.SFSObject)) *Init {
	return rewardInitWithDay(20, func(raw *sfs.SFSObject) {
		al := sfs.NewSFSObject()
		al.PutUtfString("uid", "fake-alliance-1")
		raw.PutSFSObject("alliance", al)
		fill(raw)
	})
}

func salaryInit(state, daily int32) *Init {
	return allianceInit(func(raw *sfs.SFSObject) {
		raw.PutInt("salaryActivityState", state)
		raw.PutInt("dailySalaryStatus", daily)
	})
}

// salaryInfo is an activity.info reply: weekly maps configId -> status; daily < 0 omits
// dailySalaryInfo.
func salaryInfo(daily int32, weekly map[int32]int32) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	if daily >= 0 {
		d := sfs.NewSFSObject()
		d.PutInt("configId", 101)
		d.PutInt("status", daily)
		resp.PutSFSObject("dailySalaryInfo", d)
	}
	arr := sfs.NewSFSArray()
	for _, id := range []int32{201, 202, 203, 204, 205, 301} {
		if s, ok := weekly[id]; ok {
			w := sfs.NewSFSObject()
			w.PutInt("configId", id)
			w.PutInt("status", s)
			arr.AddSFSObject(w)
		}
	}
	resp.PutSFSArray("weeklySalaryInfo", arr)
	return resp
}

func salaryRun(t *testing.T, in *Init, info *sfs.SFSObject, claim func(id int32) *sfs.SFSObject) ([]int32, *rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == allianceSalaryInfoCmd {
			return info
		}
		return claim(p.GetInt("configId"))
	})
	err := runAllianceSalary(conn, in)
	var claimed []int32
	for _, r := range fake.requests() {
		if r.Cmd == allianceSalaryClaimCmd {
			claimed = append(claimed, r.Params.GetInt("configId"))
		}
	}
	return claimed, fake, err
}

func TestAllianceSalaryClaimsDailyAndWeekly(t *testing.T) {
	ok := func(int32) *sfs.SFSObject { return rewardOK() }
	claimed, _, err := salaryRun(t, salaryInit(10, 1), salaryInfo(-1, map[int32]int32{201: 2, 202: 1, 203: 0, 301: 1}), ok)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !slices.Equal(claimed, []int32{101, 202, 301}) {
		t.Errorf("claimed %v, want [101 202 301]", claimed)
	}

	// The info reply's daily status wins over the init one.
	claimed, _, err = salaryRun(t, salaryInit(10, 1), salaryInfo(2, nil), ok)
	if err != nil || len(claimed) != 0 {
		t.Errorf("claimed %v (err %v), want nothing: dailySalaryInfo says already taken", claimed, err)
	}
}

func TestAllianceSalaryGates(t *testing.T) {
	ok := func(int32) *sfs.SFSObject { return rewardOK() }
	for name, in := range map[string]*Init{
		"no init":           {},
		"no alliance":       rewardInitWithDay(20, func(raw *sfs.SFSObject) { raw.PutInt("salaryActivityState", 10); raw.PutInt("dailySalaryStatus", 1) }),
		"activity not open": salaryInit(0, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, fake, err := salaryRun(t, in, salaryInfo(1, nil), ok)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
		})
	}
	_, fake, err := salaryRun(t, salaryInit(10, 0), salaryInfo(-1, map[int32]int32{201: 0}), ok)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{allianceSalaryInfoCmd}) {
		t.Errorf("sent %v, want only the info read", got)
	}
}

func TestAllianceSalaryBenignAndFailure(t *testing.T) {
	claimed, _, err := salaryRun(t, salaryInit(10, 1), salaryInfo(-1, map[int32]int32{201: 1}), func(id int32) *sfs.SFSObject {
		if id == 101 {
			return rewardErr("120289")
		}
		return rewardErr("999999")
	})
	if err == nil {
		t.Error("run = nil, want the weekly claim failure")
	}
	if !slices.Equal(claimed, []int32{101, 201}) {
		t.Errorf("claimed %v, want both tried", claimed)
	}
}
