package game

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// wishPool is one init lotteryFreeInfo[] pool with its wish fields (LotteryInfo.lua:284-295).
type wishPool struct {
	id         string
	poolType   int32 // 0: omitted
	wishSwitch bool
	need, cur  int32 // wishPityReceiveNum, wishPityCurNum
	hero       string
	start, end int64 // seconds
}

func wishInit(pools ...wishPool) *Init {
	in := evInit(30)
	arr := sfs.NewSFSArray()
	for _, p := range pools {
		o := sfs.NewSFSObject()
		o.PutUtfString("id", p.id)
		if p.poolType != 0 {
			o.PutInt("type", p.poolType)
		}
		o.PutBool("wishSwitch", p.wishSwitch)
		o.PutInt("wishPityReceiveNum", p.need)
		o.PutInt("wishPityCurNum", p.cur)
		o.PutUtfString("wishHero", p.hero)
		o.PutLong("startTime", p.start)
		o.PutLong("endTime", p.end)
		arr.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("lotteryFreeInfo", arr)
	return in
}

// TestHeroWishClaimClaimsOnlyFullPitiesWithAChosenHero is MASTER 1.0.370 G24: the wish entrance's
// claim sends hero.lottery.receive.wish {id: UtfString} only when IsCanClaimWish holds and a wish
// hero is chosen (UIHeroRecruitWishEntranceComponent.lua:118-127; LotteryInfo.lua:798-803), on a
// pool the view lists and shows the entrance for (IsShowWish, :468-478).
func TestHeroWishClaimClaimsOnlyFullPitiesWithAChosenHero(t *testing.T) {
	withEvNow(t, evTestNow)
	now := evTestNow.Unix()
	in := wishInit(
		wishPool{id: "221501", wishSwitch: true, need: 200, cur: 200, hero: "100005"},              // full, hero chosen
		wishPool{id: "221602", wishSwitch: true, need: 200, cur: 230, hero: "100007"},              // over full
		wishPool{id: "221702", wishSwitch: true, need: 200, cur: 199, hero: "100005"},              // one short
		wishPool{id: "221703", wishSwitch: true, need: 200, cur: 200},                              // no wish hero
		wishPool{id: "221704", wishSwitch: false, need: 200, cur: 200, hero: "100005"},             // wish off
		wishPool{id: "221705", wishSwitch: true, need: 0, cur: 0, hero: "100005"},                  // no pity
		wishPool{id: "221500", wishSwitch: true, need: 200, cur: 200, hero: "100005"},              // no table wish list
		wishPool{id: "221706", poolType: 2, wishSwitch: true, need: 200, cur: 200, hero: "100005"}, // not listed
		wishPool{id: "221707", wishSwitch: true, need: 200, cur: 200, hero: "100005", start: now + 3600, end: now + 7200},
		wishPool{id: "221501", wishSwitch: true, need: 200, cur: 200, hero: "100005"}, // duplicate
	)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroWishClaim(conn, in); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range fake.requests() {
		if r.Cmd != heroWishClaimCmd {
			t.Errorf("sent %s, want only %s (never hero.lottery.switch.wish)", r.Cmd, heroWishClaimCmd)
			continue
		}
		if v, _ := r.Params.Get("id"); v.Type != sfs.SFSUtfString {
			t.Errorf("id type %d, want UtfString", v.Type)
		}
		if keys := session.ParamKeysWithoutID(r.Params); !slices.Equal(keys, []string{"id"}) {
			t.Errorf("params %v, want only id", keys)
		}
		ids = append(ids, r.Params.GetString("id"))
	}
	if want := []string{"221501", "221602"}; !slices.Equal(ids, want) {
		t.Errorf("claimed %v, want %v", ids, want)
	}
}

// TestHeroWishClaimLogsAndStopsOnError: the first reply is logged at Info; an errorCode stops the
// claims (no benign code is registered for a never-sent command); no init sends nothing.
func TestHeroWishClaimLogsAndStopsOnError(t *testing.T) {
	withEvNow(t, evTestNow)
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	in := wishInit(
		wishPool{id: "221501", wishSwitch: true, need: 200, cur: 200, hero: "100005"},
		wishPool{id: "221602", wishSwitch: true, need: 200, cur: 200, hero: "100007"},
	)
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroWishClaim(conn, in); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "first hero.lottery.receive.wish reply"); n != 1 {
		t.Errorf("logged the first reply %d times, want once:\n%s", n, buf.String())
	}

	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("120289") })
	if err := runHeroWishClaim(conn, in); err == nil {
		t.Error("run = nil, want the refusal")
	}
	if n := len(fake.cmds()); n != 1 {
		t.Errorf("sent %d claims, want 1: a refusal stops the run", n)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroWishClaim(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v", err, fake.cmds())
	}
}

func TestHeroWishClaimIsOptInAndStaticOnly(t *testing.T) {
	f, ok := featureRegistry[heroWishClaimFeature]
	if !ok {
		t.Fatal("hero-wish-claim is not registered")
	}
	if f.DefaultOn {
		t.Error("DefaultOn; a new claim must be opt-in until validated live")
	}
	if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("Summary = %q, want it to end with (static-only, not validated live)", f.Summary)
	}
	if len(f.Duel) != 0 {
		t.Errorf("Duel = %v: claiming the wish hero is no recruit and scores nothing", f.Duel)
	}
}
