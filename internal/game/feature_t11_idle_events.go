package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// T11 idle expedition surprise box and finished task events (gap T11, MASTER.md §3.2 #6),
// OPT-IN, claims only.
//
// The idle.game.main {} reply (the same read the t11-idle-expedition feature sends, gated the same
// way) carries surpriseBoxGainTime and canReceiveEventNum at the top level, and again under
// changeLevel.idleGameMain after a level change (T11IdleGameMainData:UpdateData,
// T11IdleGameMainData.lua:26-55; OnGetIdleGameMainMessage, T11IdleGameDataManager.lua:97-112).
//
//   - Surprise box: HasSurpriseBox is surpriseBoxGainTime > 0 (T11IdleGameMainData.lua:103-105).
//     The box window's Claim button sends idle.game.surprise.open {} with no cost
//     (UILWT11IdleGameSurpriseBoxView.lua:260-268; T11IdleGameSurpriseOpenMessage.lua;
//     T11IdleGameDataManager.lua:244-257 adds msg.reward).
//   - Task events: with canReceiveEventNum > 0 (the T11 entry's red dot,
//     IsShowEventRedPointByMainMsg, T11IdleGameDataManager.lua:381-386) the event list's
//     idle.game.event.all {} returns events[{uuid, eventId, status, ...}] (:421-436;
//     T11IdleGameEventData.lua:27-56). An event in status 2 (Constant.TaskState.CanReceive,
//     T11IdleGameIdleBattleConstant.lua:237-241) is claimed with idle.game.event.receive
//     {uuid: Long} (IdleGameEventReceiveMessage.lua:4-7), from the detail view's CheckPlayPlot
//     (UIIdleGameTaskEventDetailView.lua:443-458); the end plot before it is a local cutscene
//     (TryPlayPlot, T11IdleGameDataManager.lua:584-609). Every quest type's button takes that path
//     for CanReceive (UIIdleGameTaskEventDetailView.lua:282-376).
//
// Never sent: idle.game.end/start (ending or starting a run), idle.game.challenge, and the event
// goods/battle/help/delete/get commands (each spends items, fights, or reaches other players).
// The reward pool is the t11-idle-expedition feature's. A box exists only after the player ends a
// run, and events only come from runs the player starts, so this mostly picks up what the player
// left unopened (dce F3). Static-only, not validated live.
const (
	t11IdleEventsFeature  = "t11-idle-events"
	t11SurpriseOpenCmd    = "idle.game.surprise.open"
	t11EventAllCmd        = "idle.game.event.all"
	t11EventReceiveCmd    = "idle.game.event.receive"
	t11EventCanReceive    = 2  // Constant.TaskState.CanReceive
	t11EventMaxClaimsEach = 50 // a bound on event claims per run
)

func init() {
	registerFeature(Feature{
		Name:      t11IdleEventsFeature,
		Summary:   "OPT-IN: open the T11 idle expedition's surprise box and claim finished task events (never ends, starts or fights) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runT11IdleEvents,
	})
}

func runT11IdleEvents(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !evSwitchOn(in, "t11_idle_game_open") {
		return nil
	}
	msg, err := session.SendAndWait(conn, t11IdleEventsFeature+": main", t11MainCmd, sfs.NewSFSObject())
	if err != nil {
		if msg != nil {
			slog.Info(t11IdleEventsFeature+": not available", "errorCode", msg.Params.GetString("errorCode"))
			return nil
		}
		return err
	}
	box, events := t11MainCounts(msg.Params)
	slog.Info(t11IdleEventsFeature+": main", "surpriseBoxGainTime", box, "canReceiveEventNum", events)
	b := &evBatch{conn: conn}
	if box > 0 {
		if r := b.send(t11IdleEventsFeature+": surprise box", t11SurpriseOpenCmd, sfs.NewSFSObject()); r != nil {
			slog.Info(t11IdleEventsFeature+": surprise box opened", "granted", describeGrants(r.Params),
				"surpriseRewards", describeGrants(r.Params, "surpriseRewards"))
		}
	}
	if events > 0 && !b.dead {
		claimT11Events(b)
	}
	return b.err()
}

// t11MainCounts reads surpriseBoxGainTime and canReceiveEventNum from an idle.game.main reply; a
// changeLevel.idleGameMain value wins, as the client applies it second.
func t11MainCounts(p *sfs.SFSObject) (box, events int64) {
	box, _ = evNum(p, "surpriseBoxGainTime")
	events, _ = evNum(p, "canReceiveEventNum")
	if m := evObject(evObject(p, "changeLevel"), "idleGameMain"); m != nil {
		if n, ok := evNum(m, "surpriseBoxGainTime"); ok {
			box = n
		}
		if n, ok := evNum(m, "canReceiveEventNum"); ok {
			events = n
		}
	}
	return box, events
}

func claimT11Events(b *evBatch) {
	list := b.send(t11IdleEventsFeature+": task events", t11EventAllCmd, sfs.NewSFSObject())
	if list == nil {
		return
	}
	var due []*sfs.SFSObject
	all := evObjects(list.Params, "events")
	for _, e := range all {
		if st, _ := evNum(e, "status"); st == t11EventCanReceive {
			due = append(due, e)
		}
	}
	slog.Info(t11IdleEventsFeature+": task events", "events", len(all), "canReceive", len(due))
	for i, e := range due {
		if i >= t11EventMaxClaimsEach {
			slog.Info(t11IdleEventsFeature+": per-run claim bound reached", "left", len(due)-i)
			return
		}
		uuid, ok := evNum(e, "uuid")
		if !ok {
			continue
		}
		eventID, _ := evNum(e, "eventId")
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		r := b.send(fmt.Sprintf("%s: task event %d", t11IdleEventsFeature, uuid), t11EventReceiveCmd, p)
		if r == nil {
			return // logged by SendAndWait and kept in b; the rest wait for a validated command
		}
		slog.Info(t11IdleEventsFeature+": task event claimed", "uuid", uuid, "eventId", eventID,
			"replyUuid", evString(r.Params, "uuid"), "granted", describeGrants(r.Params))
	}
}
