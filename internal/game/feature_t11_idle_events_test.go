package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// t11EventsMain is an idle.game.main reply with the T11IdleGameMainData fields this feature reads.
func t11EventsMain(box int64, canReceive int32) *sfs.SFSObject {
	r := t11Main(false)
	r.PutLong("surpriseBoxGainTime", box)
	r.PutInt("canReceiveEventNum", canReceive)
	return r
}

// t11EventList is an idle.game.event.all reply: events[{uuid, eventId, status}] (T11IdleGameEventData).
func t11EventList(events ...[2]int64) *sfs.SFSObject { // {uuid, status}
	r := sfs.NewSFSObject()
	a := sfs.NewSFSArray()
	for _, e := range events {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", e[0])
		o.PutInt("eventId", 7001)
		o.PutInt("status", int32(e[1]))
		a.AddSFSObject(o)
	}
	r.PutSFSArray("events", a)
	return r
}

// t11Forbidden are commands this feature must never send.
var t11Forbidden = []string{
	"idle.game.end", "idle.game.start", "idle.game.challenge", "idle.game.reward.receive",
	"idle.game.event.goods", "idle.game.event.battle", "idle.game.event.help", "idle.game.event.delete",
	"idle.game.event.get", "idle.game.event.plot",
}

func runT11EventsFake(t *testing.T, main, list *sfs.SFSObject, receive func(p *sfs.SFSObject) *sfs.SFSObject) *evFake {
	t.Helper()
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case t11MainCmd:
			return main
		case t11EventAllCmd:
			return list
		case t11EventReceiveCmd:
			if receive != nil {
				return receive(p)
			}
		}
		return evOK()
	})
	if err := runT11IdleEvents(conn, evAllianceInit("t11_idle_game_open")); err != nil && receive == nil {
		t.Fatal(err)
	}
	for _, c := range fake.cmds() {
		if slices.Contains(t11Forbidden, c) {
			t.Errorf("sent %s; the feature only opens the box and claims finished events", c)
		}
	}
	return fake
}

func TestT11IdleEventsClaimsBoxAndCanReceiveEvents(t *testing.T) {
	const bigUUID = 9007199254740993 // 2^53+1: a Long uuid must keep every digit
	list := t11EventList([2]int64{bigUUID, 2}, [2]int64{2, 1}, [2]int64{3, 3}, [2]int64{4, 2})
	fake := runT11EventsFake(t, t11EventsMain(1791171134420, 2), list, nil)
	want := []string{t11MainCmd, t11SurpriseOpenCmd, t11EventAllCmd, t11EventReceiveCmd, t11EventReceiveCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	var uuids []int64
	for _, r := range fake.only(t11EventReceiveCmd) {
		v, _ := r.Params.Get("uuid")
		if v.Type != sfs.SFSLong {
			t.Errorf("uuid type %d, want Long", v.Type)
		}
		uuids = append(uuids, r.Params.GetLong("uuid"))
	}
	if !slices.Equal(uuids, []int64{bigUUID, 4}) {
		t.Errorf("claimed %v, want only the status-2 events [%d 4]", uuids, int64(bigUUID))
	}
	for _, c := range []string{t11SurpriseOpenCmd, t11EventAllCmd} {
		for _, k := range fake.only(c)[0].Params.Keys() {
			if k != "_id" {
				t.Errorf("%s carries %q; the client sends no params", c, k)
			}
		}
	}
}

func TestT11IdleEventsGates(t *testing.T) {
	for _, c := range []struct {
		name string
		main *sfs.SFSObject
		want []string
	}{
		{"box only", t11EventsMain(5, 0), []string{t11MainCmd, t11SurpriseOpenCmd}},
		{"events only", t11EventsMain(0, 1), []string{t11MainCmd, t11EventAllCmd, t11EventReceiveCmd}},
		{"neither", t11EventsMain(0, 0), []string{t11MainCmd}},
		{"fields absent", t11Main(true), []string{t11MainCmd}},
		{"changeLevel wins", func() *sfs.SFSObject {
			m := t11EventsMain(0, 0)
			cl := sfs.NewSFSObject()
			im := sfs.NewSFSObject()
			im.PutLong("surpriseBoxGainTime", 9)
			cl.PutSFSObject("idleGameMain", im)
			m.PutSFSObject("changeLevel", cl)
			return m
		}(), []string{t11MainCmd, t11SurpriseOpenCmd}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runT11EventsFake(t, c.main, t11EventList([2]int64{11, 2}), nil)
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestT11IdleEventsSwitchOffAndLocked(t *testing.T) {
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E_LOCKED") })
	if err := runT11IdleEvents(conn, evAllianceInit()); err != nil || len(fake.requests()) != 0 {
		t.Errorf("switch off: err %v, sent %v", err, fake.cmds())
	}
	if err := runT11IdleEvents(conn, evAllianceInit("t11_idle_game_open")); err != nil {
		t.Errorf("an errorCode on main means not available, got %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{t11MainCmd}) {
		t.Errorf("sent %v, want only main", got)
	}
}

func TestT11IdleEventsReceiveErrorStopsTheEvents(t *testing.T) {
	list := t11EventList([2]int64{1, 2}, [2]int64{2, 2})
	fake := runT11EventsFake(t, t11EventsMain(0, 2), list, func(*sfs.SFSObject) *sfs.SFSObject { return evErr(evAlreadyExecuted) })
	if got := len(fake.only(t11EventReceiveCmd)); got != 1 {
		t.Errorf("sent %d event claims, want 1: an error stops the unvalidated claim", got)
	}
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case t11MainCmd:
			return t11EventsMain(0, 1)
		case t11EventAllCmd:
			return t11EventList([2]int64{1, 2})
		}
		return evErr("E7")
	})
	if err := runT11IdleEvents(conn, evAllianceInit("t11_idle_game_open")); err == nil || !strings.Contains(err.Error(), "E7") {
		t.Errorf("err = %v, want the E7 failure returned", err)
	}
}

func TestT11IdleEventsIsOptIn(t *testing.T) {
	f, ok := featureRegistry[t11IdleEventsFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, DefaultOn false, no duel scores, static-only summary", f)
	}
}
