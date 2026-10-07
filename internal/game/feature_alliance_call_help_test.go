package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

type helpSent struct {
	uuid, kind, qType int64
	itemID            string
}

// callHelpInit is a timerInit inside an alliance.
func callHelpInit(c timerInit, inAlliance bool) *Init {
	in := c.build()
	if inAlliance {
		al := sfs.NewSFSObject()
		al.PutUtfString("uid", "fake-alliance-1")
		in.Raw.PutSFSObject("alliance", al)
	}
	return in
}

func runCallHelp(t *testing.T, in *Init, reply *sfs.SFSObject) ([]helpSent, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return reply })
	err := runAllianceCallHelp(conn, in)
	var out []helpSent
	for _, r := range fake.requests() {
		if r.Cmd != callHelpCmd {
			t.Errorf("unexpected cmd %q", r.Cmd)
		}
		item, _ := r.Params.Get("itemId")
		s, _ := item.Val.(string)
		out = append(out, helpSent{r.Params.GetLong("uuid"), int64(r.Params.GetInt("type")), int64(r.Params.GetInt("qType")), s})
	}
	return out, err
}

func TestAllianceCallHelpAsksOnRunningTimersOnly(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Minute)
	c := timerInit{
		buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: future, help: 0},    // ask: upgrade
			{uuid: 2, bID: 10202000, lv: 5, uT: future, help: 1},    // upgrade already helped
			{uuid: 3, bID: 10202000, lv: 5, uT: future, help: 2},    // ask: only the repair was helped
			{uuid: 4, bID: 10202000, lv: 5, uT: past},               // finished, nothing to ask
			{uuid: 5, bID: 10107000, lv: 5, dEndT: future, help: 0}, // ask: repair
			{uuid: 6, bID: 10107000, lv: 5, dEndT: future, help: 2}, // repair already helped
		},
		queues: []timerQueue{
			{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: future, itemID: "1101001"}, // ask: research
			{uuid: 22, qType: 6, start: now.Add(-time.Hour), end: future, isHelped: 1},       // already asked
			{uuid: 23, qType: 3, start: now.Add(-time.Hour), end: future},                    // ask: healing
			{uuid: 24, qType: 117, start: now.Add(-time.Hour), end: past},                    // finished
			{uuid: 25, qType: 1, start: now.Add(-time.Hour), end: future},                    // not a helpable queue
		},
		soldiers: -1,
	}
	sent, err := runCallHelp(t, callHelpInit(c, true), rewardOK())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []helpSent{{1, 0, 0, ""}, {3, 0, 0, ""}, {5, 2, 0, ""}, {21, 1, 6, "1101001"}, {23, 1, 3, ""}}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
}

func TestAllianceCallHelpGates(t *testing.T) {
	running := timerInit{buildings: []timerBuilding{{uuid: 1, bID: 10202000, lv: 5, uT: time.Now().Add(time.Hour)}}}
	for name, in := range map[string]*Init{
		"no init":     {},
		"no alliance": callHelpInit(running, false),
		"nothing running": callHelpInit(timerInit{buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: time.Now().Add(-time.Hour)},
		}}, true),
	} {
		t.Run(name, func(t *testing.T) {
			sent, err := runCallHelp(t, in, rewardOK())
			if err != nil || len(sent) != 0 {
				t.Errorf("err %v, sent %v; want nothing", err, sent)
			}
		})
	}
}

func TestAllianceCallHelpBenignAndFailure(t *testing.T) {
	running := timerInit{buildings: []timerBuilding{
		{uuid: 1, bID: 10202000, lv: 5, uT: time.Now().Add(time.Hour)},
		{uuid: 2, bID: 10202000, lv: 5, uT: time.Now().Add(time.Hour)},
	}}
	if _, err := runCallHelp(t, callHelpInit(running, true), rewardErr("120289")); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	sent, err := runCallHelp(t, callHelpInit(running, true), rewardErr("999999"))
	if err == nil || len(sent) != 2 {
		t.Errorf("err %v, sent %v; want an error after trying both", err, sent)
	}
}
