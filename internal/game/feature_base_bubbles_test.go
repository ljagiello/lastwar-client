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
// received long ago (500550), one expired (500551) and a scout bubble (500525, never); the
// player's own HQ with a Thanksgiving status (own base) and a Valentine one (520526); a
// stranger's HQ; ally B's HQ moved off its home server; and ally B's second claimable status.
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

// bbInit is fwTestInit with the given activities open, the receive dict and the day's count.
func bbInit(today int32, acts ...int32) *Init {
	in := fwTestInit()
	m := in.Object("activity")
	for _, id := range acts {
		m.PutSFSObject(strconv.Itoa(int(id)), evActivity(id))
	}
	recv := sfs.NewSFSArray()
	for _, s := range []string{
		fwAllyA + "_500550_1000", // long expired, still received for a Thanksgiving status
		"broken-entry",
	} {
		recv.AddValue(sfs.SFSValue{Type: sfs.SFSUtfString, Val: s})
	}
	in.Raw.PutSFSArray("activityReceiveArr", recv)
	tt := sfs.NewSFSArray()
	o := sfs.NewSFSObject()
	o.PutInt("99302", today)
	tt.AddSFSObject(o)
	in.Raw.PutSFSArray("thanksgiving_times", tt)
	in.Raw.PutLong("thanksgiving_times_expire", evTestNow.Add(14*time.Hour).UnixMilli())
	return in
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
	if len(recv) != 1 {
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

func TestBaseBubblesClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startBbFake(t, func(_ string, p *sfs.SFSObject) *sfs.SFSObject {
		r := fwClaimOK(p)
		tt := sfs.NewSFSArray()
		o := sfs.NewSFSObject()
		o.PutInt("99302", 4)
		tt.AddSFSObject(o)
		r.PutSFSArray("thanksgiving_times", tt)
		return r
	})
	if err := runBaseBubbles(conn, bbInit(3, 99302, 99137)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block,world.get.block,"+valentineOwnerClaimCmd+","+thanksgivingClaimCmd {
		t.Fatalf("sent %s", got)
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
	if len(*slept) != 1 {
		t.Errorf("paused %v", *slept)
	}
}

func TestBaseBubblesDayCapAndGates(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	ok := func(_ string, p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) }
	cases := []struct {
		name string
		in   *Init
		want []string
	}{
		{"the day's 10 are taken", bbInit(10, 99302, 99137), []string{valentineOwnerClaimCmd}},
		{"no Valentine activity", bbInit(0, 99302), []string{thanksgivingClaimCmd}},
		{"the 2024 Thanksgiving row", bbInit(0, 99108), []string{thanksgivingClaimCmd}},
		{"nothing open", bbInit(0), nil},
		{"another activity only", bbInit(0, 99011), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startBbFake(t, ok)
			if err := runBaseBubbles(conn, c.in); err != nil {
				t.Fatal(err)
			}
			var claims []string
			for _, cmd := range fake.cmds() {
				if cmd == thanksgivingClaimCmd || cmd == valentineOwnerClaimCmd {
					claims = append(claims, cmd)
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
	conn, fake := startBbFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E_RECEIVED") })
	err := runBaseBubbles(conn, bbInit(0, 99302, 99137))
	if err == nil || !strings.Contains(err.Error(), "E_RECEIVED") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.only(valentineOwnerClaimCmd)) + len(fake.only(thanksgivingClaimCmd)); n != 1 {
		t.Errorf("sent %d claims; the first refusal ends the run", n)
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
