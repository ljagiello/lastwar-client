package game

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// pbHQStatus builds a player HQ point with statuses (BuildInfo field 32) and both server ids.
func pbHQStatus(pointID int32, owner string, server, src int32, statuses ...worldStatus) []byte {
	var bi []byte
	bi = pbBytesField(bi, 1, []byte(owner))
	bi = pbVarintField(bi, 2, int64(pointID)*7)
	bi = pbVarintField(bi, 3, claimHQBuildingID)
	bi = pbBytesField(bi, 7, []byte(fwAlliance))
	bi = pbBytesField(bi, 14, []byte("name of "+owner))
	for _, s := range statuses {
		bi = pbBytesField(bi, 32, pbStatus(s))
	}
	var p []byte
	p = pbVarintField(p, 1, int64(pointID))
	p = pbVarintField(p, 2, 1)
	p = pbBytesField(p, 3, bi)
	p = pbVarintField(p, 102, int64(server))
	return pbVarintField(p, 103, int64(src))
}

// bbStatus is status id running from an hour ago until expiresIn after evTestNow.
func bbStatus(id int32, expiresIn time.Duration) worldStatus {
	return worldStatus{ID: id, BeginTime: evTestNow.Add(-time.Hour).UnixMilli(), ExpireTime: evTestNow.Add(expiresIn).UnixMilli(), Layer: 1}
}

// bbBlockReply holds: ally A's HQ with a claimable Thanksgiving status (500521), one already
// received long ago (500550), one expired (500551) and a scout bubble (500525: never sent; bbInit
// lists it as received, so it does not take the tap); the player's own HQ with a Thanksgiving
// status (own base) and a Valentine one (520526); a stranger's HQ; and ally B's HQ moved off its
// home server.
func bbBlockReply() *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQStatus(347520, fwAllyA, fwServer, fwServer,
		bbStatus(500521, 10*time.Hour), bbStatus(500550, 10*time.Hour), bbStatus(500551, -time.Minute), bbStatus(500525, 10*time.Hour)))
	pts.AddByteArray(pbHQStatus(347513, fwSelf, fwServer, fwServer, bbStatus(500522, 5*time.Hour), bbStatus(520526, 5*time.Hour)))
	pts.AddByteArray(pbHQStatus(347515, fwStranger, fwServer, fwServer, bbStatus(500521, 10*time.Hour)))
	pts.AddByteArray(pbHQStatus(500001, fwAllyB, fwServer, 999, bbStatus(500521, 10*time.Hour)))
	r := sfs.NewSFSObject()
	r.PutSFSArray("points", pts)
	r.PutLong("timeStamp", evTestNow.UnixMilli())
	return r
}

// setBbReceived sets init activityReceiveArr to entries ("<ownerUid>_<statusId>_<expire s>").
func setBbReceived(in *Init, entries ...string) {
	recv := sfs.NewSFSArray()
	for _, s := range entries {
		recv.AddValue(sfs.SFSValue{Type: sfs.SFSUtfString, Val: s})
	}
	in.Raw.PutSFSArray("activityReceiveArr", recv)
}

// bbInit is fwTestInit with the given activities open, the receive dict and the day's count. The
// receive dict lists ally A's Thanksgiving 500550 and scout 500525, both long expired (statuses
// without an activity_id count as received anyway), and a malformed entry.
func bbInit(today int32, acts ...int32) *Init {
	in := fwTestInit()
	m := in.Object("activity")
	for _, id := range acts {
		m.PutSFSObject(strconv.Itoa(int(id)), evActivity(id))
	}
	setBbReceived(in, fwAllyA+"_500550_1000", fwAllyA+"_500525_1000", "broken-entry")
	tt := sfs.NewSFSArray()
	o := sfs.NewSFSObject()
	o.PutInt("99302", today)
	tt.AddSFSObject(o)
	in.Raw.PutSFSArray("thanksgiving_times", tt)
	in.Raw.PutLong("thanksgiving_times_expire", evTestNow.Add(14*time.Hour).UnixMilli())
	return in
}

// bbScoutUnreceived is bbInit without the scout pair: ally A's bubble then targets the scout.
func bbScoutUnreceived(today int32, acts ...int32) *Init {
	in := bbInit(today, acts...)
	setBbReceived(in, fwAllyA+"_500550_1000", "broken-entry")
	return in
}

// bbInfoReply is a thanksgiving.info reply with the fields ActGiftGivingData:ParseData reads
// (ActGiftGivingData.lua:20-40); givePlayers names other players, so the logs must not.
func bbInfoReply(p *sfs.SFSObject) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("activityId", p.GetInt("activityId"))
	r.PutInt("totalAmount", 12)
	gp := sfs.NewSFSArray()
	for _, uid := range []string{fwLauncher, fwStranger} {
		o := sfs.NewSFSObject()
		o.PutUtfString("uid", uid)
		o.PutUtfString("name", "Name of "+uid)
		gp.AddSFSObject(o)
	}
	r.PutSFSArray("givePlayers", gp)
	ra := sfs.NewSFSArray()
	ra.AddInt(0)
	r.PutSFSArray("receiveArr", ra)
	return r
}

// bbReplies answers thanksgiving.info with bbInfoReply and each claim with a reward and the day's
// Thanksgiving count today; any other command is refused.
func bbReplies(today int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case thanksgivingInfoCmd:
			return bbInfoReply(p)
		case thanksgivingClaimCmd, valentineOwnerClaimCmd:
			r := fwClaimOK(p)
			tt := sfs.NewSFSArray()
			o := sfs.NewSFSObject()
			o.PutInt("99302", today)
			tt.AddSFSObject(o)
			r.PutSFSArray("thanksgiving_times", tt)
			return r
		}
		return evErr("unexpected command")
	}
}

func startBbFake(t *testing.T, reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	blocks := fwBlockAnswer(t, bbBlockReply())
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case fireworksRankCmd:
			return fwRankReply()
		case fireworksBlockCmd:
			return blocks()
		}
		return reply(cmd, p)
	})
}

func TestBaseBubbleVerdicts(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startBbFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	in := bbInit(0, 99302, 99137)
	sc, err := sharedAllianceTileScan(conn, in, "test")
	if err != nil || sc == nil || len(sc.bases) != 4 {
		t.Fatalf("scan = %+v, %v", sc, err)
	}
	recv := newBaseReceiveDict(in)
	if len(recv) != 2 {
		t.Errorf("receive dict %v; the broken entry must be skipped", recv)
	}
	want := map[string]string{
		"347520/500521": "", "347520/500550": "already received", "347520/500551": "expired", "347520/500525": "not a reward bubble",
		"347513/500522": "own base", "347513/520526": "",
		"347515/500521": "not an alliance member's base",
		"500001/500521": "not on its owner's home server",
	}
	n := 0
	for _, b := range sc.bases {
		for _, s := range b.Statuses {
			key := strconv.Itoa(int(b.PointID)) + "/" + strconv.Itoa(int(s.ID))
			if got := sc.bubbleVerdict(b, s, recv, sc.now); got != want[key] {
				t.Errorf("%s: verdict %q, want %q", key, got, want[key])
			}
			n++
		}
	}
	if n != len(want) {
		t.Errorf("judged %d statuses, want %d", n, len(want))
	}
	// Valentine: the own pair received and unexpired, or the day cap of 7 unexpired pairs.
	own := sc.bases[slices.IndexFunc(sc.bases, func(b fwBase) bool { return b.OwnerUID == fwSelf })]
	val := bbStatus(520526, 5*time.Hour)
	later := evTestNow.Add(time.Hour).Unix()
	if got := sc.bubbleVerdict(own, val, baseReceiveDict{{fwSelf, 520526}: later}, sc.now); got != "already received" {
		t.Errorf("own pair received: %q", got)
	}
	if got := sc.bubbleVerdict(own, val, baseReceiveDict{{fwSelf, 520526}: evTestNow.Add(-time.Hour).Unix()}, sc.now); got != "" {
		t.Errorf("own pair expired: %q", got)
	}
	capped := baseReceiveDict{}
	for i := range 7 {
		capped[receivedPair{"uid-other-" + strconv.Itoa(i), 520526}] = later
	}
	if got := sc.bubbleVerdict(own, val, capped, sc.now); got != "day cap reached" {
		t.Errorf("day cap: %q", got)
	}
}

// A base's type2-31 and type2-33 statuses share one bubble whose tap targets the highest type2
// not yet received (BuildingRewardBubbleEffect.lua:150-160): an unexpired, unreceived scout
// status takes the tap (a scout march, :220-221), so its base's Thanksgiving status is not claimed.
func TestBaseBubbleScoutTakesTheTap(t *testing.T) {
	withEvNow(t, evTestNow)
	sc := &fwScan{ownUID: fwSelf, server: fwServer, now: evTestNow,
		roster: fwRoster{byUID: map[string]fwMember{fwAllyA: {}, fwSelf: {}}}}
	tg := bbStatus(500521, 10*time.Hour)
	allyA := func(statuses ...worldStatus) fwBase {
		return fwBase{PointID: 347520, ServerID: fwServer, SrcServerID: fwServer, OwnerUID: fwAllyA,
			Statuses: append([]worldStatus{tg}, statuses...)}
	}
	cases := []struct {
		name string
		base fwBase
		recv baseReceiveDict
		want string
	}{
		{"no scout status", allyA(), baseReceiveDict{}, ""},
		{"an unreceived 500525", allyA(bbStatus(500525, 10*time.Hour)), baseReceiveDict{}, "a scout bubble takes the tap first"},
		{"an unreceived 500554", allyA(bbStatus(500554, time.Hour)), baseReceiveDict{}, "a scout bubble takes the tap first"},
		{"the scout received long ago", allyA(bbStatus(500525, 10*time.Hour)), baseReceiveDict{{fwAllyA, 500525}: 1000}, ""},
		{"the scout expired", allyA(bbStatus(500525, -time.Minute)), baseReceiveDict{}, ""},
		{"the scout received on another base", allyA(bbStatus(500525, 10*time.Hour)), baseReceiveDict{{fwAllyB, 500525}: 1000},
			"a scout bubble takes the tap first"},
		{"the Thanksgiving status itself received", allyA(bbStatus(500525, 10*time.Hour)), baseReceiveDict{{fwAllyA, 500521}: 1000},
			"already received"},
	}
	for _, c := range cases {
		if got := sc.bubbleVerdict(c.base, tg, c.recv, sc.now); got != c.want {
			t.Errorf("%s: verdict %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBaseBubblesClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	logs := captureFwLogs(t)
	conn, fake := startBbFake(t, bbReplies(4))
	if err := runBaseBubbles(conn, bbInit(3, 99302, 99137)); err != nil {
		t.Fatal(err)
	}
	// thanksgiving.info goes before the first Thanksgiving claim, once.
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block,world.get.block,"+valentineOwnerClaimCmd+","+
		thanksgivingInfoCmd+","+thanksgivingClaimCmd {
		t.Fatalf("sent %s", got)
	}
	info := fake.only(thanksgivingInfoCmd)[0].Params
	if keys := session.ParamKeysWithoutID(info); !slices.Equal(keys, []string{"activityId"}) || info.GetInt("activityId") != 99302 {
		t.Errorf("thanksgiving info %s", info.StringRedacted())
	}
	if v, _ := info.Get("activityId"); v.Type != sfs.SFSInt {
		t.Errorf("activityId type %v", v.Type)
	}
	out := logs.String()
	for _, want := range []string{"replyActivityId=99302", "totalAmount=12", "givePlayers=2", "receiveArr=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the info log lacks %q", want)
		}
	}
	for _, uid := range fwAllUIDs {
		if strings.Contains(out, uid) {
			t.Errorf("the log contains player uid %s", uid)
		}
	}
	val := fake.only(valentineOwnerClaimCmd)[0].Params
	if keys := session.ParamKeysWithoutID(val); !slices.Equal(keys, []string{"activityId", "statusId"}) ||
		val.GetInt("activityId") != 99137 || val.GetInt("statusId") != 520526 {
		t.Errorf("valentine claim %s", val.StringRedacted())
	}
	tg := fake.only(thanksgivingClaimCmd)[0].Params
	if keys := session.ParamKeysWithoutID(tg); !slices.Equal(keys, []string{"activityId", "otherUid", "statusId"}) ||
		tg.GetInt("activityId") != 99302 || tg.GetString("otherUid") != fwAllyA || tg.GetInt("statusId") != 500521 {
		t.Errorf("thanksgiving claim %s", tg.StringRedacted())
	}
	if v, _ := tg.Get("otherUid"); v.Type != sfs.SFSUtfString {
		t.Errorf("otherUid type %v", v.Type)
	}
	if len(*slept) != 2 {
		t.Errorf("paused %v; one pause before each request after the first", *slept)
	}
}

// An errorCode on thanksgiving.info leaves the client without the activity's data, so no
// Thanksgiving bubble shows: no Thanksgiving claim, and the Valentine own-base claim goes on.
func TestBaseBubblesThanksgivingInfoRefused(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	for _, c := range []struct {
		name string
		in   *Init
		want string
	}{
		{"with Valentine", bbInit(0, 99302, 99137), valentineOwnerClaimCmd + "," + thanksgivingInfoCmd},
		{"Thanksgiving only", bbInit(0, 99302), thanksgivingInfoCmd},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok := bbReplies(0)
			conn, fake := startBbFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == thanksgivingInfoCmd {
					return evErr("E_INFO")
				}
				return ok(cmd, p)
			})
			err := runBaseBubbles(conn, c.in)
			if err == nil || !strings.Contains(err.Error(), "E_INFO") {
				t.Errorf("err = %v", err)
			}
			if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block,world.get.block,"+c.want {
				t.Errorf("sent %s", got)
			}
		})
	}
}

func TestBaseBubblesDayCapAndGates(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	tgClaim := []string{thanksgivingInfoCmd, thanksgivingClaimCmd}
	cases := []struct {
		name string
		in   *Init
		act  int32 // the Thanksgiving activity of the info and claim
		want []string
	}{
		{"the day's 10 are taken", bbInit(10, 99302, 99137), 0, []string{valentineOwnerClaimCmd}},
		{"no Valentine activity", bbInit(0, 99302), 99302, tgClaim},
		{"the 2024 Thanksgiving row", bbInit(0, 99108), 99108, tgClaim},
		{"a scout bubble on ally A's base", bbScoutUnreceived(0, 99302, 99137), 0, []string{valentineOwnerClaimCmd}},
		{"nothing open", bbInit(0), 0, nil},
		{"another activity only", bbInit(0, 99011), 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startBbFake(t, bbReplies(0))
			if err := runBaseBubbles(conn, c.in); err != nil {
				t.Fatal(err)
			}
			var claims []string
			for _, r := range fake.requests() {
				switch r.Cmd {
				case thanksgivingInfoCmd, thanksgivingClaimCmd:
					if got := r.Params.GetInt("activityId"); got != c.act {
						t.Errorf("%s activityId %d, want %d", r.Cmd, got, c.act)
					}
					claims = append(claims, r.Cmd)
				case valentineOwnerClaimCmd:
					claims = append(claims, r.Cmd)
				}
			}
			if !slices.Equal(claims, c.want) {
				t.Errorf("claims %v, want %v (sent %v)", claims, c.want, fake.cmds())
			}
			if c.want == nil && len(fake.cmds()) != 0 {
				t.Errorf("sent %v with no activity open", fake.cmds())
			}
		})
	}
	if got := fake99108Activity(t); got != 99108 {
		t.Errorf("open activity %d", got)
	}
}

// fake99108Activity checks openActivityOf picks the lowest open id.
func fake99108Activity(t *testing.T) int32 {
	t.Helper()
	id, _ := openActivityOf(bbInit(0, 99302, 99108), thanksgivingActivities(), thanksgivingActivityType)
	return id
}

func TestBaseBubblesStopOnErrorCode(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	ok := bbReplies(0)
	conn, fake := startBbFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == valentineOwnerClaimCmd || cmd == thanksgivingClaimCmd {
			return evErr("E_RECEIVED")
		}
		return ok(cmd, p)
	})
	err := runBaseBubbles(conn, bbInit(0, 99302, 99137))
	if err == nil || !strings.Contains(err.Error(), "E_RECEIVED") {
		t.Errorf("err = %v", err)
	}
	// The Valentine bubble expires first, so its refusal ends the run before the Thanksgiving part.
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block,world.get.block,"+valentineOwnerClaimCmd {
		t.Errorf("sent %s; the first refusal ends the run", got)
	}
}

func TestBaseBubblesFeature(t *testing.T) {
	f, ok := featureRegistry[baseBubblesFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasPrefix(f.Summary, "OPT-IN") ||
		!strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("registered %v, DefaultOn %v, summary %q", ok, f.DefaultOn, f.Summary)
	}
	if len(thanksgivingStatuses) != 4 || valentineStatuses[520533].activityID != 99192 {
		t.Error("lw_status constants")
	}
	// Table activity 40413's type-321 (ActGiftGiving) rows are 99108 and 99302; both are in the
	// generated table, which the lookup reads.
	if got := thanksgivingActivities(); !slices.Equal(got, []int32{99108, 99302}) {
		t.Errorf("thanksgivingActivities() = %v, want [99108 99302]", got)
	}
}

// bbTwoOnOneBase holds ally A's HQ with two claimable Thanksgiving statuses whose expiries run
// opposite to their ids (500521 in 10 h, 500522 in 5 h), and the player's own HQ with a Valentine
// bubble (520526) that expires after both.
func bbTwoOnOneBase() *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQStatus(347520, fwAllyA, fwServer, fwServer, bbStatus(500521, 10*time.Hour), bbStatus(500522, 5*time.Hour)))
	pts.AddByteArray(pbHQStatus(347513, fwSelf, fwServer, fwServer, bbStatus(520526, 12*time.Hour)))
	r := sfs.NewSFSObject()
	r.PutSFSArray("points", pts)
	r.PutLong("timeStamp", evTestNow.UnixMilli())
	return r
}

// TestBaseBubblesOrderAndStops covers wp7-r2 #1 and #2 on bbTwoOnOneBase:
//   - the bubble over one base taps its statuses by type2 descending, then id ascending
//     (BuildingRewardBubbleEffect.lua:150-155), so 500521 is claimed before 500522 although it
//     expires later, and thanksgiving.info goes out once before both;
//   - a refused thanksgiving.info skips the Thanksgiving claims but the Valentine own-base claim
//     still goes out;
//   - a refused Thanksgiving claim ends the run: nothing follows it.
func TestBaseBubblesOrderAndStops(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	ok := bbReplies(0)
	for _, c := range []struct {
		name    string
		refuse  func(cmd string, p *sfs.SFSObject) bool
		want    []string // the commands after the scan, Thanksgiving claims as "claim <statusId>"
		wantErr bool
	}{
		{"all answered", func(string, *sfs.SFSObject) bool { return false },
			[]string{thanksgivingInfoCmd, "claim 500521", "claim 500522", valentineOwnerClaimCmd}, false},
		{"info refused", func(cmd string, _ *sfs.SFSObject) bool { return cmd == thanksgivingInfoCmd },
			[]string{thanksgivingInfoCmd, valentineOwnerClaimCmd}, true},
		{"first claim refused", func(cmd string, p *sfs.SFSObject) bool {
			return cmd == thanksgivingClaimCmd && p.GetInt("statusId") == 500521
		}, []string{thanksgivingInfoCmd, "claim 500521"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			blocks := fwBlockAnswer(t, bbTwoOnOneBase())
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch {
				case cmd == fireworksRankCmd:
					return fwRankReply()
				case cmd == fireworksBlockCmd:
					return blocks()
				case c.refuse(cmd, p):
					return evErr("E_REFUSED")
				}
				return ok(cmd, p)
			})
			err := runBaseBubbles(conn, bbInit(0, 99302, 99137))
			if (err != nil) != c.wantErr {
				t.Errorf("run = %v, want an error %v", err, c.wantErr)
			}
			var got []string
			for _, r := range fake.requests() {
				switch r.Cmd {
				case fireworksRankCmd, fireworksBlockCmd:
				case thanksgivingClaimCmd:
					got = append(got, "claim "+strconv.Itoa(int(r.Params.GetInt("statusId"))))
				default:
					got = append(got, r.Cmd)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestBaseBubblesRefusedNextToClaimed: each case changes one thing of the claimed fixture (ally A's
// HQ with Thanksgiving status 500521, the player's HQ with Valentine status 520526, Thanksgiving
// 99302 and Valentine 99137 open, today's count 0) and the claim it gates is not sent.
func TestBaseBubblesRefusedNextToClaimed(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	allyA := func(server int32, statuses ...worldStatus) []byte {
		return pbHQStatus(347520, fwAllyA, server, server, statuses...)
	}
	tg, val := bbStatus(500521, 10*time.Hour), bbStatus(520526, 5*time.Hour)
	block := aBbBlock(allyA(fwServer, tg), pbHQStatus(347513, fwSelf, fwServer, fwServer, val))
	opened := []int32{99302, 99137}
	received := []string{fwAllyA + "_500550_1000"}
	today := []map[string]int32{{"99302": 0}}
	closed := func(id int32) func(*Init) {
		return func(in *Init) {
			a := evActivity(id)
			a.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
			a.PutLong("endViewTime", evTestNow.Add(-time.Hour).UnixMilli())
			in.Object("activity").PutSFSObject(strconv.Itoa(int(id)), a)
		}
	}
	both := []string{"val 520526", "info", "tg 500521"}
	for _, c := range []struct {
		name  string
		block *sfs.SFSObject
		in    *Init
		edit  func(*Init)
		want  []string
	}{
		{"both claimed", block, aBbInit(opened, received, today, true), nil, both},
		// BuildingGetRewardBubbleEffect.lua:130-140: the Valentine bubble is the player's own base's.
		{"the Valentine status on ally A's HQ", aBbBlock(allyA(fwServer, tg, val), pbHQStatus(347513, fwSelf, fwServer, fwServer)),
			aBbInit(opened, received, today, true), nil, []string{"info", "tg 500521"}},
		// CityDomeProtectEffectManager.lua:1147-1165: serverId == srcServerId == the player's server.
		{"ally A's HQ at home on server 999", aBbBlock(allyA(999, tg), pbHQStatus(347513, fwSelf, fwServer, fwServer, val)),
			aBbInit(opened, received, today, true), nil, []string{"val 520526"}},
		// GetOneOpenActId walks open activities only (ActGiftGivingDataManager.lua:263-293).
		{"Thanksgiving 99302 listed but ended", block, aBbInit(opened, received, today, true), closed(99302), []string{"val 520526"}},
		{"Valentine 99137 listed but ended", block, aBbInit(opened, received, today, true), closed(99137), []string{"info", "tg 500521"}},
		// The count sits in the second thanksgiving_times map; the client merges every map
		// (SetRewardBubbleGetNumData, ActGiftGivingDataManager.lua:221-232), so the day's 10 are used.
		{"today's 10 in the second map", block, aBbInit(opened, received, []map[string]int32{{"99108": 0}, {"99302": 10}}, true), nil,
			[]string{"val 520526"}},
		// Init lists today's 10 without thanksgiving_times_expire. The client would read 0 here
		// (expire starts at 0 and only a present key sets it, ActGiftGivingDataManager.lua:17,
		// 229-231, 245-247); Go keeps the listed count, so a run never passes day_receive_num on
		// such an init.
		{"today's 10 without the expire key", block, aBbInit(opened, received, []map[string]int32{{"99302": 10}}, false), nil,
			[]string{"val 520526"}},
		// The own pair is listed until an hour from now: received today (the dict's expire counts
		// for Valentine, BuildingGetRewardBubbleEffect.lua:130-140).
		{"the own Valentine pair received", block,
			aBbInit(opened, append(slices.Clone(received), fwSelf+"_520526_"+strconv.FormatInt(evTestNow.Add(time.Hour).Unix(), 10)), today, true),
			nil, []string{"info", "tg 500521"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.edit != nil {
				c.edit(c.in)
			}
			got := aBbClaims(t, c.block, c.in, func(_ int, p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
			if !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestBaseBubblesInRunDayCount: ally A's HQ carries three claimable Thanksgiving statuses. Each
// claim counts toward day_receive_num 10: the reply's thanksgiving_times when it is higher, else one
// more, so a run that starts at 8 claims two whatever the replies say, and a reply at 10 ends it.
func TestBaseBubblesInRunDayCount(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	block := aBbBlock(pbHQStatus(347520, fwAllyA, fwServer, fwServer,
		bbStatus(500521, 10*time.Hour), bbStatus(500522, 9*time.Hour), bbStatus(500551, 8*time.Hour)))
	two := []string{"info", "tg 500521", "tg 500522"}
	for _, c := range []struct {
		name  string
		start int32
		reply func(n int) (int32, bool) // the count a claim reply carries, if any
		want  []string
	}{
		{"replies without the count", 8, func(int) (int32, bool) { return 0, false }, two},
		{"replies with the count before the claim", 8, func(int) (int32, bool) { return 8, true }, two},
		{"replies with the count after the claim", 8, func(n int) (int32, bool) { return 8 + int32(n), true }, two},
		{"a reply at the day's 10", 0, func(int) (int32, bool) { return 10, true }, []string{"info", "tg 500521"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := aBbInit([]int32{99302}, nil, []map[string]int32{{"99302": c.start}}, true)
			got := aBbClaims(t, block, in, func(n int, p *sfs.SFSObject) *sfs.SFSObject {
				r := fwClaimOK(p)
				if v, ok := c.reply(n); ok {
					tt := sfs.NewSFSArray()
					tt.AddSFSObject(obj("99302", v))
					r.PutSFSArray("thanksgiving_times", tt)
				}
				return r
			})
			if !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestBaseBubblesRunCap: three members' HQs carry twelve Thanksgiving statuses and the player's HQ
// a Valentine one. The day's count stops the Thanksgiving claims at 10, and the run's cap of 10
// claims (baseBubblesMaxClaims, feature_base_bubbles.go:78) leaves the eleventh unsent.
func TestBaseBubblesRunCap(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	four := func(point int32, owner string, h time.Duration) []byte {
		return pbHQStatus(point, owner, fwServer, fwServer, bbStatus(500521, h), bbStatus(500522, h), bbStatus(500550, h), bbStatus(500551, h))
	}
	block := aBbBlock(four(347520, fwAllyA, 10*time.Hour), four(500001, fwAllyB, 11*time.Hour), four(123456, fwAway, 12*time.Hour),
		pbHQStatus(347513, fwSelf, fwServer, fwServer, bbStatus(520526, 5*time.Hour)))
	in := aBbInit([]int32{99302, 99137}, nil, []map[string]int32{{"99302": 0}}, true)
	got := aBbClaims(t, block, in, func(_ int, p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
	claims := slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s == "info" })
	if len(claims) != 10 || claims[0] != "val 520526" {
		t.Errorf("claimed %v, want the Valentine bubble and nine Thanksgiving ones (10)", claims)
	}
}

// TestBaseBubbleValentineNeedsTheOwnUID: a base with no owner is not the player's, also when init
// names no uid (BuildingGetRewardBubbleEffect.lua:130-140 compares the owner with the player's uid).
func TestBaseBubbleValentineNeedsTheOwnUID(t *testing.T) {
	val := bbStatus(520526, 5*time.Hour)
	for _, c := range []struct {
		name, own, owner string
		want             bool
	}{
		{"the player's HQ", fwSelf, fwSelf, true},
		{"an ownerless base, own uid unknown", "", "", false},
		{"an ownerless base", fwSelf, "", false},
	} {
		sc := &fwScan{ownUID: c.own, server: fwServer, now: evTestNow, roster: fwRoster{byUID: map[string]fwMember{fwSelf: {}}}}
		b := fwBase{PointID: 347513, ServerID: fwServer, SrcServerID: fwServer, OwnerUID: c.owner, Statuses: []worldStatus{val}}
		if got := sc.bubbleVerdict(b, val, baseReceiveDict{}, evTestNow) == ""; got != c.want {
			t.Errorf("%s: claimable %v, want %v", c.name, got, c.want)
		}
	}
}
