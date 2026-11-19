package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// duelGachaInit is an init in an alliance, with switch alliance_duel_zhuanpan on, season 6 on its
// day 30 and effect 92019 at 1, each changeable by edit.
func duelGachaInit(edit func(raw *sfs.SFSObject)) *Init {
	in := evInit(30)
	raw := in.Raw
	raw.PutSFSObject("alliance", duelObj("uid", "a1"))
	raw.PutSFSObject("dataConfig", duelObj(duelGachaSwitch, 1))
	raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 6, "seasonStartTime", evTestNow.Add(-29*24*time.Hour).UnixMilli()))
	raw.PutSFSObject("effect", duelObj("92019", 1.0))
	if edit != nil {
		edit(raw)
	}
	return in
}

// duelGachaInfo is an al.duel.lottery.get.info reply as AllyDuelScoreGachaManager:
// OnReceiveConfigData stores it: configId, score, protect {num, protectNum, reward[]}, posArray.
func duelGachaInfo(score, pity, counter int64, rewards int) *sfs.SFSObject {
	r := duelObj("configId", 3, "score", score)
	protect := duelObj("num", pity, "protectNum", counter)
	rw := sfs.NewSFSArray()
	for i := range rewards {
		rw.AddSFSObject(duelObj("type", 7, "itemId", 200000+i, "num", 1))
	}
	protect.PutSFSArray("reward", rw)
	r.PutSFSObject("protect", protect)
	pos := sfs.NewSFSArray()
	pos.AddSFSObject(duelObj("pos", 1, "itemId", "200000", "num", 1))
	r.PutSFSArray("posArray", pos)
	return r
}

func runDuelGachaFake(t *testing.T, in *Init, info *sfs.SFSObject, dry bool) ([]evReq, error) {
	t.Helper()
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case duelGachaInfoCmd:
			return info
		case duelGachaPityCmd:
			return duelObj("protectNum", 0, "reward", "x")
		}
		return evErr("999999")
	})
	err := duelGachaRun(conn, in, dry)
	for _, r := range fake.requests() {
		if r.Cmd != duelGachaInfoCmd && r.Cmd != duelGachaPityCmd {
			t.Errorf("sent %s: only the gacha read and the pity claim may be sent (never a draw)", r.Cmd)
		}
	}
	return fake.requests(), err
}

func TestDuelGachaRegistration(t *testing.T) {
	for _, name := range []string{duelGachaFeature, duelGachaPlanFeature} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || f.Hold || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s registered as %+v; want opt-in, not duel-held, static-only", name, f)
		}
	}
}

func TestDuelGachaClaimsAFullPity(t *testing.T) {
	reqs, err := runDuelGachaFake(t, duelGachaInit(nil), duelGachaInfo(25_000_000, 50, 50, 1), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].Cmd != duelGachaInfoCmd || reqs[1].Cmd != duelGachaPityCmd {
		t.Fatalf("sent %v, want the info read then the pity claim", reqs)
	}
	if len(reqs[0].Params.Keys()) > 1 { // the info read has no params (only the _id the conn adds)
		t.Errorf("info params %v, want none", reqs[0].Params)
	}
	v, _ := reqs[1].Params.Get("configId")
	if v.Type != sfs.SFSInt || v.Val != int32(3) {
		t.Errorf("pity claim configId %v (type %d), want Int 3", v.Val, v.Type)
	}
}

func TestDuelGachaClaimsNothingElse(t *testing.T) {
	for name, c := range map[string]struct {
		in   *Init
		info *sfs.SFSObject
		sent []string
	}{
		"pity not full":   {duelGachaInit(nil), duelGachaInfo(99_000_000, 50, 49, 1), []string{duelGachaInfoCmd}},
		"no pity":         {duelGachaInit(nil), duelGachaInfo(0, 0, 0, 1), []string{duelGachaInfoCmd}},
		"no wish reward":  {duelGachaInit(nil), duelGachaInfo(0, 50, 50, 0), []string{duelGachaInfoCmd}},
		"no configId":     {duelGachaInit(nil), duelWithoutKey(duelGachaInfo(0, 50, 50, 1), "configId"), []string{duelGachaInfoCmd}},
		"effect 92019 0":  {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("effect", duelObj("92019", 0.0)) }), duelGachaInfo(0, 50, 50, 1), []string{duelGachaInfoCmd}},
		"no alliance":     {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("alliance", duelObj("uid", "")) }), duelGachaInfo(0, 50, 50, 1), nil},
		"switch off":      {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("dataConfig", duelObj(duelGachaSwitch, 0)) }), duelGachaInfo(0, 50, 50, 1), nil},
		"no switch":       {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("dataConfig", sfs.NewSFSObject()) }), duelGachaInfo(0, 50, 50, 1), nil},
		"season 0":        {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 0)) }), duelGachaInfo(0, 50, 50, 1), nil},
		"season 1 day 56": {duelGachaInit(duelGachaSeason1(55)), duelGachaInfo(0, 50, 50, 1), nil},
		"season 1 day 57": {duelGachaInit(duelGachaSeason1(56)), duelGachaInfo(0, 50, 50, 1), []string{duelGachaInfoCmd, duelGachaPityCmd}},
		"season 1, no start time": {duelGachaInit(func(raw *sfs.SFSObject) {
			raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 1))
		}), duelGachaInfo(0, 50, 50, 1), nil},
	} {
		reqs, err := runDuelGachaFake(t, c.in, c.info, false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		var got []string
		for _, r := range reqs {
			got = append(got, r.Cmd)
		}
		if !slices.Equal(got, c.sent) {
			t.Errorf("%s: sent %v, want %v", name, got, c.sent)
		}
	}
}

// duelGachaSeason1 puts season 1, started daysAgo days before evTestNow (its day daysAgo+1).
func duelGachaSeason1(daysAgo int) func(raw *sfs.SFSObject) {
	return func(raw *sfs.SFSObject) {
		start := evTestNow.Add(-time.Duration(daysAgo) * 24 * time.Hour).UnixMilli()
		raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 1, "seasonStartTime", start))
	}
}

func TestDuelGachaInfoErrorStops(t *testing.T) {
	// No benign codes: an errorCode on the never-sent read is a failure and nothing follows it.
	reqs, err := runDuelGachaFake(t, duelGachaInit(nil), evErr("E100"), false)
	if err == nil || !strings.Contains(err.Error(), "E100") {
		t.Errorf("err = %v, want the E100 failure", err)
	}
	if len(reqs) != 1 {
		t.Errorf("sent %v, want only the info read", reqs)
	}
}

func TestDuelGachaLockedAccountReply(t *testing.T) {
	// The likely first live reply on an account whose gacha is locked: no configId, no protect.
	reqs, err := runDuelGachaFake(t, duelGachaInit(nil), duelObj("score", 0), false)
	if err != nil || len(reqs) != 1 || reqs[0].Cmd != duelGachaInfoCmd {
		t.Errorf("sent %v (err %v), want only the info read and no error", reqs, err)
	}
}

func TestDuelGachaPlanOnlyReads(t *testing.T) {
	reqs, err := runDuelGachaFake(t, duelGachaInit(nil), duelGachaInfo(25_000_000, 50, 50, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].Cmd != duelGachaInfoCmd {
		t.Errorf("plan sent %v, want only the info read", reqs)
	}
}
