package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// eventGapFeatures are the opt-in claims the 1.0.370 audit's WP8 adds (MASTER.md G1, G4, G8, G9,
// G15, G25, G26, G30).
var eventGapFeatures = []string{
	sunriseFeature, giftBoxDrawFeature, giftBoxBoxesFeature, heroExchangeFeature, worldBossKillsFeature,
	foodBoxesFeature, foodBoxesFeature + foodPlanSuffix, banquetV2Feature, banquetV2Feature + foodPlanSuffix,
	bountyMilestonesFeature, bountyChestsFeature, eventMilestonesFeature, decorationFeature,
}

// TestEventGapFeaturesAreOptInAndStaticOnly: every new claim is registered, off by default, says it
// was not validated live, and is a plain claim (no duel scoring, no hold, not a capacity claim).
func TestEventGapFeaturesAreOptInAndStaticOnly(t *testing.T) {
	for _, name := range eventGapFeatures {
		f, ok := featureRegistry[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if f.DefaultOn {
			t.Errorf("%s is on by default; new claims stay opt-in until validated live", name)
		}
		if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s summary %q must end with (static-only, not validated live)", name, f.Summary)
		}
		if !strings.HasPrefix(f.Summary, "OPT-IN: ") {
			t.Errorf("%s summary %q must start with OPT-IN", name, f.Summary)
		}
		if featurePhase(name) == phaseCapacity || len(f.Duel) > 0 || f.Hold {
			t.Errorf("%s: phase %d, duel %v, hold %v; want a plain claim", name, featurePhase(name), f.Duel, f.Hold)
		}
	}
}

// G1: sunrise.receive.reward registers no benign code: even 120289 is an error that stops the
// feature (IMPLEMENT rule 8).
func TestSunriseClaimErrorIsNotBenign(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == sunriseInfoCmd {
			return sunriseInfo(0, [3]int32{2, 0, 0})
		}
		return evErr(evAlreadyExecuted)
	})
	err := runSunrise(conn, evInit(30, evActivity(94310), evActivity(94312)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake.only(sunriseRewardCmd)); n != 1 {
		t.Errorf("claims = %d, want 1: an error stops the feature", n)
	}
}

// sunriseInfo is a sunrise.info reply: unlock and rewardArr rows {level, rewardFlag,
// specialRewardFlag}.
func sunriseInfo(unlock int32, rows ...[3]int32) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("activityId", 94310)
	r.PutInt("unlock", unlock)
	arr := sfs.NewSFSArray()
	for _, row := range rows {
		o := sfs.NewSFSObject()
		o.PutInt("level", row[0])
		o.PutInt("rewardFlag", row[1])
		o.PutInt("specialRewardFlag", row[2])
		arr.AddSFSObject(o)
	}
	r.PutSFSArray("rewardArr", arr)
	return r
}

// G1: a row at or below the HQ level with the free reward unclaimed (or, with the track bought, the
// paid one) sends one claim-all (ActSunriseFoundationData.lua:41-66; UILWSunriseFoundation.lua:
// 318-324).
func TestSunriseClaimsReachedRows(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name   string
		hq     int32
		unlock int32
		rows   [][3]int32
		want   bool
	}{
		{"free row reached", 10, 0, [][3]int32{{2, 1, 1}, {10, 0, 1}}, true},
		{"free row above the HQ", 9, 0, [][3]int32{{2, 1, 1}, {10, 0, 1}}, false},
		{"only paid rows left, not bought", 30, 0, [][3]int32{{2, 1, 0}, {4, 1, 0}}, false},
		{"only paid rows left, bought", 30, 1, [][3]int32{{2, 1, 0}}, true},
		{"everything claimed", 30, 1, [][3]int32{{2, 1, 1}}, false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == sunriseInfoCmd {
				return sunriseInfo(c.unlock, c.rows...)
			}
			return evOK()
		})
		if err := runSunrise(conn, evInit(c.hq, evActivity(94310))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		info := fake.only(sunriseInfoCmd)
		if len(info) != 1 {
			t.Fatalf("%s: sunrise.info sent %d times, want 1", c.name, len(info))
		}
		if v, _ := info[0].Params.Get("activityId"); v.Type != sfs.SFSInt || info[0].Params.GetInt("activityId") != 94310 {
			t.Errorf("%s: info activityId %#v, want Int 94310", c.name, v)
		}
		claims := fake.only(sunriseRewardCmd)
		if (len(claims) == 1) != c.want {
			t.Errorf("%s: claims %d, want claimed=%v", c.name, len(claims), c.want)
			continue
		}
		if c.want {
			if v, _ := claims[0].Params.Get("activityId"); v.Type != sfs.SFSInt || claims[0].Params.GetInt("activityId") != 94310 {
				t.Errorf("%s: claim activityId %#v, want Int 94310", c.name, v)
			}
		}
	}
}

// G1: an info error stops the feature (sunrise.info is new to Go), and a closed activity is not
// read.
func TestSunriseInfoErrorStopsAndClosedIsSkipped(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E9") })
	err := runSunrise(conn, evInit(30, evActivity(94310), evActivity(94312)))
	if err == nil || !strings.Contains(err.Error(), "E9") {
		t.Errorf("err = %v, want the E9 failure", err)
	}
	if got := strings.Join(fake.cmds(), ","); got != sunriseInfoCmd {
		t.Errorf("sent %s, want one info read before stopping", got)
	}
	closed := evActivity(94310)
	closed.PutLong("endTime", evTestNow.Add(-240*time.Hour).UnixMilli())
	conn2, fake2 := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSunrise(conn2, evInit(30, closed)); err != nil || len(fake2.requests()) != 0 {
		t.Errorf("closed activity: err %v, sent %v", err, fake2.cmds())
	}
}
