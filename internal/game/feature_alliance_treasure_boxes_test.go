package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// atBoxFix is a siege box (WorldTreasureType 2, world_treasure row 3: 10 claims, no daily limit)
// of the player's alliance and another one, near ally A's HQ.
func atBoxFix(uuid int64, typ int32, eventID string, edit func(*atFix)) atFix {
	f := atFix{pointID: 347545, pointType: atPointTypeTreasure, pointUUID: uuid, server: fwServer, uuid: uuid,
		alliance: "al-other;" + fwAlliance, eventID: eventID, typ: typ, expiresIn: 2 * time.Hour,
		claimers: []string{atClaimerA, atClaimerB}}
	if edit != nil {
		edit(&f)
	}
	return f
}

// atBoxFixtures are the box points on the tiles, with the verdict each must get under
// atBoxTestInit's daily counts.
var atBoxFixtures = []struct {
	f   atFix
	why string
}{
	{atBoxFix(8001, 2, "3", nil), ""},
	{atBoxFix(8002, 8, "201", func(f *atFix) { f.claimers = atMany(3) }), "full"}, // row 201 holds 3 claims
	{atBoxFix(8003, 5, "14", func(f *atFix) { f.alliance = "al-other" }), "another alliance's box"},
	{atBoxFix(8004, 8, "101", nil), "daily limit reached"}, // 2 of row 101's daily 2 claimed today
	{atBoxFix(8005, 8, "102", nil), ""},                    // 2 of 102's, but yesterday
	{atBoxFix(8006, 2, "3", func(f *atFix) { f.claimers = []string{atClaimerA, fwSelf} }), "already claimed by you"},
	{atBoxFix(8007, 2, "99999", func(f *atFix) { f.claimers = atMany(9) }), ""}, // not in the table: 10 claims
	{atBoxFix(8008, 2, "99999", func(f *atFix) { f.claimers = atMany(10) }), "full"},
	{atBoxFix(8009, 2, "3", func(f *atFix) { f.server = 999 }), "on another server"},
	{atBoxFix(8010, 2, "3", func(f *atFix) { f.expiresIn = -time.Minute }), "expired"},
	{atBoxFix(8011, 8, "203", func(f *atFix) { f.alliance = fwAlliance; f.expiresIn = 30 * time.Minute }), ""},
	{atBoxFix(8012, 2, "3", func(f *atFix) { f.alliance = "" }), "another alliance's box"},
}

// atBoxTestInit is fwTestInit with init treasureDailyLimit: row 101 claimed twice today, row 102
// twice yesterday (before the 02:00 UTC day start).
func atBoxTestInit() *Init {
	in := fwTestInit()
	arr := sfs.NewSFSArray()
	for _, e := range []struct {
		id, num int32
		at      time.Time
	}{{101, 2, evTestNow.Add(-time.Hour)}, {102, 2, evTestNow.Add(-12 * time.Hour)}} {
		o := sfs.NewSFSObject()
		o.PutInt("id", e.id)
		o.PutInt("num", e.num)
		o.PutLong("time", e.at.UnixMilli())
		arr.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("treasureDailyLimit", arr)
	return in
}

// atBoxBlockReply holds ally A's HQ, the box fixtures, a radar treasure and a sandworm box
// (type 7), which this feature leaves alone.
func atBoxBlockReply(serverNow time.Time) *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance))
	for _, c := range atBoxFixtures {
		pts.AddByteArray(pbTreasurePoint(c.f))
	}
	pts.AddByteArray(pbTreasurePoint(atDug(7001, 3*time.Hour)))
	pts.AddByteArray(pbTreasurePoint(atBoxFix(8100, 7, "30", nil)))
	r := sfs.NewSFSObject()
	r.PutSFSArray("points", pts)
	r.PutLong("timeStamp", serverNow.UnixMilli())
	return r
}

func TestAllianceTreasureBoxVerdicts(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startAtFake(t, atBoxBlockReply(evTestNow), nil)
	in := atBoxTestInit()
	sc, err := sharedAllianceTileScan(conn, in, "test")
	if err != nil || sc == nil {
		t.Fatalf("scan = %v, %v", sc, err)
	}
	daily := newAtBoxDaily(in)
	if daily[101] != 2 || daily.reached(102, worldTreasureRows[102]) {
		t.Fatalf("daily counts %v", daily)
	}
	byUUID := map[int64]string{}
	for _, pt := range sc.treasures {
		if atBoxTypes[pt.Treasure.Type] != "" {
			byUUID[sc.box(pt).uuid] = sc.boxVerdict(sc.box(pt), daily, nil)
		}
	}
	if len(byUUID) != len(atBoxFixtures) {
		t.Errorf("reviewed %d boxes, want %d (no radar treasure, no sandworm box)", len(byUUID), len(atBoxFixtures))
	}
	for _, c := range atBoxFixtures {
		if got := byUUID[c.f.uuid]; got != c.why {
			t.Errorf("box %d: verdict %q, want %q", c.f.uuid, got, c.why)
		}
	}
	if r := worldTreasureRows[201]; r.maxReward != 3 || r.dailyMax != 1 || len(worldTreasureRows) != 98 ||
		worldTreasureRows[605].group != 602 || worldTreasureRows[1].maxReward != 2 {
		t.Errorf("world_treasure rows: 201 %+v, %d rows", r, len(worldTreasureRows))
	}
	// A group's count stands for every row of the group (row 605 is in group 602, daily 10).
	if g := (atBoxDaily{602: 10}); !g.reached(605, worldTreasureRows[605]) || g.reached(703, worldTreasureRows[703]) {
		t.Error("group daily limit")
	}
}

func TestAllianceTreasureBoxesClaim(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startAtFake(t, atBoxBlockReply(evTestNow), func(_ int, p *sfs.SFSObject) *sfs.SFSObject {
		r := atClaimOK(p)
		r.PutInt("cfgId", 3)
		r.PutInt("treasureRewardLimit", 1)
		return r
	})
	if err := runAllianceTreasureBoxes(conn, atBoxTestInit()); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(allianceTreasureClaimCmd)
	var got []int64
	for _, c := range claims {
		got = append(got, c.Params.GetLong("uuid"))
		if keys := session.ParamKeysWithoutID(c.Params); !slices.Equal(keys, []string{"uuid", "targetServer"}) || c.Params.GetInt("targetServer") != fwServer {
			t.Errorf("claim params %s", c.Params.StringRedacted())
		}
	}
	// 8011 despawns first; then 8001, 8005 and 8007 (same expiry, by uuid).
	if !slices.Equal(got, []int64{8011, 8001, 8005, 8007}) {
		t.Errorf("claimed %v", got)
	}
	if len(*slept) != 3 {
		t.Errorf("paused %v between claims", *slept)
	}
}

func TestAllianceTreasureBoxesDailyLimitInRun(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	pts := sfs.NewSFSArray()
	for _, uuid := range []int64{8201, 8202} {
		pts.AddByteArray(pbTreasurePoint(atBoxFix(uuid, 8, "201", nil)))
	}
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("points", pts)
	conn, fake := startAtFake(t, blk, func(_ int, p *sfs.SFSObject) *sfs.SFSObject {
		r := atClaimOK(p)
		r.PutInt("cfgId", 201)
		r.PutInt("treasureRewardLimit", 1) // row 201's daily limit is 1
		return r
	})
	if err := runAllianceTreasureBoxes(conn, atBoxTestInit()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(allianceTreasureClaimCmd)); n != 1 {
		t.Errorf("sent %d claims; the first claim's treasureRewardLimit reaches row 201's daily 1", n)
	}
}

func TestAllianceTreasureBoxesStopOnAnyErrorCode(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	for _, code := range []string{"801348", "E_DAILY"} {
		t.Run(code, func(t *testing.T) {
			conn, fake := startAtFake(t, atBoxBlockReply(evTestNow), func(int, *sfs.SFSObject) *sfs.SFSObject { return evErr(code) })
			err := runAllianceTreasureBoxes(conn, atBoxTestInit())
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("err = %v", err)
			}
			if n := len(fake.only(allianceTreasureClaimCmd)); n != 1 {
				t.Errorf("sent %d claims; the first refusal ends the run", n)
			}
		})
	}
}

func TestAllianceTreasureBoxesFeature(t *testing.T) {
	f, ok := featureRegistry[atBoxFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasPrefix(f.Summary, "OPT-IN") ||
		!strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("%s: registered %v, DefaultOn %v, summary %q", atBoxFeature, ok, f.DefaultOn, f.Summary)
	}
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	for _, in := range []*Init{nil, {}} {
		if err := runAllianceTreasureBoxes(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v without init", fake.cmds())
	}
}
