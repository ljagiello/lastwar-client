package game

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Arms Race and Alliance Duel stage chests (MASTER.md §4 #11). Static-only: the claim params are
// read from the Lua senders, but no claim has been sent live, and whether a chest survives its
// stage end is not visible in the Lua. Validate live before enabling.
//
// Arms Race is activity type 125 (PersonalArmsNew; type 12 has no table rows). Its state comes
// from activity.hero.get.info {aid: Int} (ActivityListDataManager.lua:2682-2707), parsed by
// ActivityPersonalArmsDataManager:UpdateData (:18-64): sc, score_rewards[{target, receive}],
// day_rewards[{resourceNum, receive}], resourceItemNum, stage_end_time (seconds).
//   - Score chests: activity.hero.score.reward {aid, index: -1} claims every reached one
//     (PersonalArms.lua:722; GetScoreBoxState :201-221).
//   - Daily chests: activity.hero.day.reward {aid, index: i-1}, one per call, when resourceItemNum
//     reaches resourceNum (GetDailyBoxState :186-200; PersonalArms.lua:705).
//
// Alliance Duel is activity 55000 (type 14). Its hero.event.info.get reply carries eventList[];
// the client keeps the last entry (ActAllianceBattleInfo.lua:27-35, ActivityEventInfo.lua:65-360)
// and claims chest i with accept.personal.reward {actId: UtfString, stage: Int targetList[i],
// eventType: Int 15} while score >= targetList[i], i is not in newRewardFlagList (1-based CSV) and
// i is unlocked (3, 6, 9 or 12 chests by effects 92001/92002/92018,
// AllianceCompeteDataManager.lua:373-388; AllyDuelToday.lua:680-697).
const (
	armsRaceInfoCmd        = "activity.hero.get.info"
	armsRaceScoreRewardCmd = "activity.hero.score.reward"
	armsRaceDayRewardCmd   = "activity.hero.day.reward"
	allianceDuelRewardCmd  = "accept.personal.reward"
	allianceDuelEventType  = 15 // EnumActivity.AllianceCompete.EventType (EnumType.lua:6266-6270)
)

// armsRaceGroups is table `activity`'s group_number/group_priority for the type-125 rows that have
// one (1.0.364): within a group the client keeps only the highest priority present in init.
var armsRaceGroups = map[int32][2]int32{24: {100, 10}, 26: {100, 20}, 27: {100, 30}, 29: {100, 40}}

// allianceDuelUnlocks maps the Duel chest-unlock effects to the chest count each grants,
// highest first.
var allianceDuelUnlocks = []struct{ effect, chests int }{{92018, 12}, {92002, 9}, {92001, 6}}

func init() {
	registerFeature(Feature{
		Name:    "arms-race-chests",
		Summary: "claim reached Arms Race score/daily chests and Alliance Duel chests; static-only, VALIDATE LIVE FIRST",
		Run:     runArmsRaceChests,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, armsRaceScoreRewardCmd, armsRaceDayRewardCmd, allianceDuelRewardCmd)
}

func runArmsRaceChests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range armsRaceActivities(s) {
		if b.dead {
			break
		}
		claimArmsRace(s, b, a)
	}
	for _, a := range s.Open(actTypeAllianceCompete) {
		if b.dead {
			break
		}
		claimAllianceDuel(s, b, in, a)
	}
	return b.err()
}

// armsRaceActivities is the open type-125 activities after the client's group rule
// (HandleAddPersonalArmsNew): an activity loses to a higher-priority one of its group.
func armsRaceActivities(s *ActivitySweep) []Activity {
	all := s.All(actTypePersonalArmsNew)
	var out []Activity
	for _, a := range s.Open(actTypePersonalArmsNew) {
		g := armsRaceGroups[a.ID]
		superseded := false
		for _, o := range all {
			og := armsRaceGroups[o.ID]
			if o.ID != a.ID && og[0] == g[0] && og[1] > g[1] {
				superseded = true
			}
		}
		if !superseded {
			out = append(out, a)
		}
	}
	return out
}

func armsRaceInfo(s *ActivitySweep, a Activity) (*sfs.SFSObject, error) {
	p := sfs.NewSFSObject()
	p.PutInt("aid", a.ID)
	return s.Info("arms race info response", armsRaceInfoCmd, a, p)
}

func claimArmsRace(s *ActivitySweep, b *evBatch, a Activity) {
	info, err := armsRaceInfo(s, a)
	if err != nil {
		b.add(err)
		return
	}
	// GetCurData returns nil once the stage has ended (ActivityPersonalArmsDataManager.lua:155-173).
	if end, ok := evNum(info, "stage_end_time"); ok && end > 0 && evNow().Unix() >= end {
		return
	}
	score, _ := evNum(info, "sc")
	for _, r := range evObjects(info, "score_rewards") {
		target, ok := evNum(r, "target")
		if recv, _ := evNum(r, "receive"); ok && recv != 1 && score >= target {
			p := sfs.NewSFSObject()
			p.PutInt("aid", a.ID)
			p.PutInt("index", -1)
			if b.send(fmt.Sprintf("arms race score chests (activity %d)", a.ID), armsRaceScoreRewardCmd, p) != nil {
				// The chests pay medals, which can open daily chests: re-read before those.
				s.Forget(armsRaceInfoCmd, a)
				if info, err = armsRaceInfo(s, a); err != nil {
					b.add(err)
					return
				}
			}
			break
		}
	}
	medals, _ := evNum(info, "resourceItemNum")
	for i, r := range evObjects(info, "day_rewards") {
		if b.dead {
			return
		}
		need, ok := evNum(r, "resourceNum")
		if recv, _ := evNum(r, "receive"); !ok || recv == 1 || medals < need {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		p.PutInt("index", int32(i))
		b.send(fmt.Sprintf("arms race daily chest %d (activity %d)", i, a.ID), armsRaceDayRewardCmd, p)
	}
}

func claimAllianceDuel(s *ActivitySweep, b *evBatch, in *Init, a Activity) {
	info, err := s.EventInfo(a)
	if err != nil {
		b.add(err)
		return
	}
	ev := allianceDuelCurrentEvent(info)
	if ev == nil {
		slog.Debug("alliance duel: no event in the info reply", "activity", a.ID)
		return
	}
	if t, ok := evNum(ev, "t"); !ok || t != allianceDuelEventType {
		return
	}
	actID := evString(ev, "actId")
	if actID == "" {
		actID = evString(ev, "id")
	}
	if actID == "" {
		return
	}
	userScore := evObject(ev, "userScore")
	score, _ := evNum(userScore, "score")
	claimed := map[int]bool{}
	for _, f := range strings.Split(evString(userScore, "newRewardFlagList"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			claimed[n] = true
		}
	}
	unlocked := 3
	for _, u := range allianceDuelUnlocks {
		if evInitEffect(in, u.effect) == 1 {
			unlocked = u.chests
			break
		}
	}
	targetStr := evString(ev, "target")
	if targetStr == "" {
		return
	}
	for i, t := range strings.Split(targetStr, "|") {
		idx := i + 1
		if idx > unlocked || b.dead {
			return
		}
		target, err := strconv.ParseInt(strings.TrimSpace(t), 10, 32)
		if err != nil || claimed[idx] || score < target {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("actId", actID)
		p.PutInt("stage", int32(target))
		p.PutInt("eventType", allianceDuelEventType)
		b.send(fmt.Sprintf("alliance duel chest %d (score %d)", idx, target), allianceDuelRewardCmd, p)
	}
}

// allianceDuelCurrentEvent picks today's eventList entry: the one whose window (duelEntryWindow,
// ms) contains now, else the last one, which is what the client keeps.
func allianceDuelCurrentEvent(info *sfs.SFSObject) *sfs.SFSObject {
	events := evObjects(info, "eventList")
	if len(events) == 0 {
		return nil
	}
	now := evNow().UnixMilli()
	for _, e := range events {
		if begin, end, ok := duelEntryWindow(e); ok && begin <= now && now < end {
			return e
		}
	}
	return events[len(events)-1]
}
