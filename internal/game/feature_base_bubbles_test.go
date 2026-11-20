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
	id, _ := openActivityOf(bbInit(0, 99302, 99108), thanksgivingActivities, thanksgivingActivityType)
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
}
