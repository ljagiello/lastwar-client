package game

import (
	"slices"
	"testing"
	"time"

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

// TestAllianceSalaryClaimsOnScore covers CanGetDailySalary's second rule: a score of at least
// alliance_pay[101]'s 200 while the status is not HasGot (AllianceMilitaryPayDataManager.lua:138-145).
func TestAllianceSalaryClaimsOnScore(t *testing.T) {
	ok := func(int32) *sfs.SFSObject { return rewardOK() }
	withScore := func(info *sfs.SFSObject, score int32) *sfs.SFSObject {
		info.PutInt("dailyUserSalary", score)
		return info
	}
	initScore := func(status, score int32, hasStatus bool) *Init {
		return allianceInit(func(raw *sfs.SFSObject) {
			raw.PutInt("salaryActivityState", 10)
			if hasStatus {
				raw.PutInt("dailySalaryStatus", status)
			}
			raw.PutInt("dailySalary", score)
		})
	}
	cases := []struct {
		name string
		in   *Init
		info *sfs.SFSObject
		want []int32
	}{
		{"init score 200, status 0", initScore(0, 200, true), salaryInfo(-1, nil), []int32{101}},
		{"init score 199, status 0", initScore(0, 199, true), salaryInfo(-1, nil), nil},
		{"reply score 250 over init 0", initScore(0, 0, true), withScore(salaryInfo(0, nil), 250), []int32{101}},
		{"reply score 150 over init 300", initScore(0, 300, true), withScore(salaryInfo(0, nil), 150), nil},
		{"score reached but already taken", initScore(0, 300, true), withScore(salaryInfo(2, nil), 300), nil},
		{"score reached, no status anywhere", initScore(0, 300, false), salaryInfo(-1, nil), nil},
		{"status 1 with a low score", initScore(1, 10, true), salaryInfo(-1, nil), []int32{101}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claimed, _, err := salaryRun(t, c.in, c.info, ok)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(claimed, c.want) {
				t.Errorf("claimed %v, want %v", claimed, c.want)
			}
		})
	}
}

// TestAllianceSalaryOpensAtSalaryTime covers UIAllianceDailySalaryNode.lua:68-75: NotOpen (0) with
// salaryTime passed counts as Open.
func TestAllianceSalaryOpensAtSalaryTime(t *testing.T) {
	ok := func(int32) *sfs.SFSObject { return rewardOK() }
	at := func(state int32, salaryTime time.Time) *Init {
		return allianceInit(func(raw *sfs.SFSObject) {
			raw.PutInt("salaryActivityState", state)
			raw.PutInt("dailySalaryStatus", 1)
			if !salaryTime.IsZero() {
				raw.PutLong("salaryTime", salaryTime.UnixMilli())
			}
		})
	}
	now := time.Now()
	cases := []struct {
		name string
		in   *Init
		want []int32
	}{
		{"NotOpen, salaryTime passed", at(0, now.Add(-time.Minute)), []int32{101}},
		{"NotOpen, salaryTime ahead", at(0, now.Add(time.Hour)), nil},
		{"NotOpen, no salaryTime", at(0, time.Time{}), nil},
		{"another state, salaryTime passed", at(5, now.Add(-time.Minute)), nil},
		{"Open, salaryTime ahead", at(10, now.Add(time.Hour)), []int32{101}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claimed, _, err := salaryRun(t, c.in, salaryInfo(-1, nil), ok)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(claimed, c.want) {
				t.Errorf("claimed %v, want %v", claimed, c.want)
			}
		})
	}
}
