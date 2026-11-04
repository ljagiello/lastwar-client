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

// premiumGiftEntry is an info.list1 entry with the client's field names (AllianceGiftData.lua:33-79).
func premiumGiftEntry(uuid string, receiveState int32, expires time.Time) *sfs.SFSObject {
	g := obj("uuid", uuid, "receiveState", receiveState, "groupId", "5001", "userName", "Name of a member", "type", 1)
	if !expires.IsZero() {
		g.PutLong("expirationTime", expires.UnixMilli())
	}
	return g
}

// premiumGiftList is an alliance.reward.list reply: gift level (negative omits onLevel), redPoint1
// (negative omits it) and the Premium gifts.
func premiumGiftList(level, redPoint int32, gifts ...*sfs.SFSObject) *sfs.SFSObject {
	info := obj("list1", gifts, "list2", []*sfs.SFSObject{}, "redPoint2", 0)
	if redPoint >= 0 {
		info.PutInt("redPoint1", redPoint)
	}
	r := obj("info", info, "onCurrExp", 10, "onMaxExp", 100)
	if level >= 0 {
		r.PutInt("onLevel", level)
	}
	return r
}

func premiumGiftsInit(kv ...any) *Init {
	return &Init{Raw: obj(append([]any{"alliance", obj("uid", "al-1"), "allianceNewMail", 3}, kv...)...)}
}

// withPremiumPace records the pauses between claims instead of sleeping.
func withPremiumPace(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	saved := premiumGiftsSleep
	premiumGiftsSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { premiumGiftsSleep = saved })
	return &slept
}

// runPremiumGifts answers the list with list and every claim with claim(n, params) (n from 1;
// receiveResult 1 when claim is nil).
func runPremiumGifts(t *testing.T, in *Init, list *sfs.SFSObject, claim func(n int, p *sfs.SFSObject) *sfs.SFSObject) (*evFake, error) {
	t.Helper()
	n := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case premiumGiftsListCmd:
			return list
		case premiumGiftsReceiveCmd:
			n++
			if claim != nil {
				return claim(n, p)
			}
			return obj("receiveResult", 1, "type", 1, "uuid", p.GetString("uuid"), "redPoint", 0,
				"info", []*sfs.SFSObject{obj("type", 2, "value", 100)})
		}
		return evErr("unexpected command")
	})
	err := runAlliancePremiumGifts(conn, in)
	return fake, err
}

func claimedUUIDs(fake *evFake) []string {
	var out []string
	for _, r := range fake.only(premiumGiftsReceiveCmd) {
		out = append(out, r.Params.GetString("uuid"))
	}
	return out
}

func TestAlliancePremiumGiftsIsRegisteredOptIn(t *testing.T) {
	f, ok := featureRegistry["alliance-premium-gifts"]
	if !ok {
		t.Fatal("alliance-premium-gifts is not registered")
	}
	if f.DefaultOn {
		t.Error("alliance-premium-gifts must be DefaultOn: false until validated live")
	}
	if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("Summary %q must end with the static-only marker", f.Summary)
	}
}

func TestAlliancePremiumGiftsGates(t *testing.T) {
	now := time.Now()
	gift := premiumGiftEntry("g1", 0, now.Add(time.Hour))
	list := []string{premiumGiftsListCmd}
	cases := []struct {
		name string
		in   *Init
		list *sfs.SFSObject
		want []string
	}{
		{"no init", &Init{}, premiumGiftList(10, 1, gift), nil},
		{"not in an alliance", &Init{Raw: obj("allianceNewMail", 3)}, premiumGiftList(10, 1, gift), nil},
		{"no gifts waiting in init", premiumGiftsInit("allianceNewMail", 0), premiumGiftList(10, 1, gift), nil},
		{"gift level unknown", premiumGiftsInit(), premiumGiftList(-1, 1, gift), list},
		{"gift level 15: the core's Claim All", premiumGiftsInit(), premiumGiftList(15, 1, gift), list},
		{"no Premium gift waiting", premiumGiftsInit(), premiumGiftList(10, 0, gift), list},
		{"redPoint1 absent", premiumGiftsInit(), premiumGiftList(10, -1, gift), list},
		{"gift level 14, one waiting", premiumGiftsInit(), premiumGiftList(14, 1, gift), []string{premiumGiftsListCmd, premiumGiftsReceiveCmd}},
		{"no allianceNewMail in init: list anyway", &Init{Raw: obj("alliance", obj("uid", "al-1"))}, premiumGiftList(1, 1, gift),
			[]string{premiumGiftsListCmd, premiumGiftsReceiveCmd}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withPremiumPace(t)
			fake, err := runPremiumGifts(t, c.in, c.list, nil)
			if err != nil {
				t.Fatalf("run = %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestAlliancePremiumGiftsClaimsEachClaimableGift(t *testing.T) {
	withPremiumPace(t)
	now := time.Now()
	longUUID := obj("uuid", int64(9007199254740993), "receiveState", 0, "expirationTime", now.Add(3*time.Hour).UnixMilli())
	list := premiumGiftList(7, 4,
		premiumGiftEntry("soon", 0, now.Add(time.Hour)),
		premiumGiftEntry("taken", 1, now.Add(time.Hour)),
		premiumGiftEntry("expired", 0, now.Add(-time.Minute)),
		premiumGiftEntry("no-expiry", 0, time.Time{}),
		premiumGiftEntry("", 0, now.Add(time.Hour)),
		obj("uuid", "regular", "receiveState", 0, "type", 2, "expirationTime", now.Add(time.Hour).UnixMilli()),
		premiumGiftEntry("late", 0, now.Add(2*time.Hour)),
		longUUID,
	)
	fake, err := runPremiumGifts(t, premiumGiftsInit(), list, nil)
	if err != nil {
		t.Fatalf("run = %v", err)
	}
	// Latest expiry first, as the window lists them; a Long uuid goes out as its decimal string.
	if got, want := claimedUUIDs(fake), []string{"9007199254740993", "late", "soon"}; !slices.Equal(got, want) {
		t.Errorf("claimed %v, want %v", got, want)
	}
	for _, r := range fake.only(premiumGiftsReceiveCmd) {
		uuid, _ := r.Params.Get("uuid")
		typ, _ := r.Params.Get("type")
		if uuid.Type != sfs.SFSUtfString || typ.Type != sfs.SFSInt || typ.Val != int32(1) {
			t.Errorf("claim params %s, want uuid UtfString and type Int 1", r.Params.StringRedacted())
		}
		if got := session.ParamKeysWithoutID(r.Params); !slices.Equal(got, []string{"uuid", "type"}) {
			t.Errorf("claim keys %v, want [uuid type]", got)
		}
	}
	for _, r := range fake.only(premiumGiftsListCmd) {
		if r.Params.GetInt("index") != 0 || r.Params.GetInt("len") != 1000 {
			t.Errorf("list params %v, want index 0 len 1000", r.Params)
		}
	}
}

func TestAlliancePremiumGiftsPaceAndCap(t *testing.T) {
	slept := withPremiumPace(t)
	now := time.Now()
	var gifts []*sfs.SFSObject
	for i := range premiumGiftsMaxPerRun + 5 {
		gifts = append(gifts, premiumGiftEntry("g"+strconv.Itoa(i), 0, now.Add(time.Duration(i+1)*time.Minute)))
	}
	fake, err := runPremiumGifts(t, premiumGiftsInit(), premiumGiftList(3, int32(len(gifts)), gifts...), nil)
	if err != nil {
		t.Fatalf("run = %v", err)
	}
	if n := len(fake.only(premiumGiftsReceiveCmd)); n != premiumGiftsMaxPerRun || len(*slept) != premiumGiftsMaxPerRun-1 {
		t.Fatalf("sent %d claims with %d pauses, want %d claims", n, len(*slept), premiumGiftsMaxPerRun)
	}
	for _, d := range *slept {
		if d < 400*time.Millisecond || d > time.Second {
			t.Fatalf("paused %v, want 0.4-1 s", d)
		}
	}
}

func TestAlliancePremiumGiftsStopsOnTheFirstErrorCode(t *testing.T) {
	withPremiumPace(t)
	now := time.Now()
	list := premiumGiftList(3, 3,
		premiumGiftEntry("a", 0, now.Add(3*time.Hour)),
		premiumGiftEntry("b", 0, now.Add(2*time.Hour)),
		premiumGiftEntry("c", 0, now.Add(time.Hour)))
	fake, err := runPremiumGifts(t, premiumGiftsInit(), list, func(n int, p *sfs.SFSObject) *sfs.SFSObject {
		if n == 2 {
			return evErr("120289") // not registered as benign: no reply has been seen
		}
		return obj("receiveResult", 1, "type", 1)
	})
	if err == nil || !strings.Contains(err.Error(), "120289") {
		t.Errorf("run = %v, want the 120289 refusal", err)
	}
	if got := claimedUUIDs(fake); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("claimed %v, want [a b] and a stop", got)
	}
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("120289") })
	if _, err := session.SendAndWait(conn, "claim", premiumGiftsReceiveCmd, sfs.NewSFSObject()); err == nil {
		t.Error("120289 is registered as benign for alliance.reward.receive")
	}
}

// TestAlliancePremiumGiftsStopsOnANotClaimedResult: the client acts only on receiveResult 1
// (AllianceGiftGetRewardMessage.lua:16-19). Another value, or none, stops the run after that gift.
func TestAlliancePremiumGiftsStopsOnANotClaimedResult(t *testing.T) {
	cases := map[string]func(n int) *sfs.SFSObject{
		"receiveResult 0":       func(n int) *sfs.SFSObject { return obj("receiveResult", n-1, "type", 1) }, // 0 first, then 1
		"receiveResult missing": func(n int) *sfs.SFSObject { return obj("type", 1) },
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			withPremiumPace(t)
			now := time.Now()
			list := premiumGiftList(3, 2, premiumGiftEntry("a", 0, now.Add(2*time.Hour)), premiumGiftEntry("b", 0, now.Add(time.Hour)))
			fake, err := runPremiumGifts(t, premiumGiftsInit(), list, func(n int, p *sfs.SFSObject) *sfs.SFSObject { return reply(n) })
			if err == nil || !strings.Contains(err.Error(), "receiveResult") {
				t.Errorf("run = %v, want a stop naming receiveResult", err)
			}
			if got := claimedUUIDs(fake); !slices.Equal(got, []string{"a"}) {
				t.Errorf("claimed %v, want only [a] and a stop", got)
			}
		})
	}
}

func TestAlliancePremiumGiftsLogsTheFirstReply(t *testing.T) {
	withPremiumPace(t)
	logs := captureFwLogs(t)
	list := premiumGiftList(3, 1, premiumGiftEntry("a", 0, time.Now().Add(time.Hour)))
	if _, err := runPremiumGifts(t, premiumGiftsInit(), list, nil); err != nil {
		t.Fatalf("run = %v", err)
	}
	out := logs.String()
	for _, want := range []string{"level=INFO", "first alliance.reward.receive reply", "receiveResult=1", "type=1", "uuid=a",
		"hasRedPoint=true", "rewardEntries=1", "replyKeys="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Name of a member") {
		t.Error("the log carries a gift sender's name")
	}
}
