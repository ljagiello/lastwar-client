package game

import (
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Tactical card box-point rewards and collection stage rewards (MASTER.md G14; cov-ui-store F2 and
// its verdict), two OPT-IN features, claims only. Static-only, from the 1.0.370 Lua and tables
// 40413, not sent live yet.
//
// The system opens with the season: TacticalCardUtil.IsFunctionOpen reads DataConfig
// battle_card_param.k6 ("4;2" in table item 40413) and is open when the season is above 4, or is 4
// and the season day (now - seasonStartTime) // 1 day + 1 is at least 2 (TacticalCardUtil.lua:
// 357-371; SeasonUtil.lua:286-316; the season is init playerServerSeasonInfo.seasonId and its
// seasonStartTime, SeasonDataManager.lua:1091-1103, SeasonInfoTemplate.lua:17-23).
//
//   - tactical-card-box-points: battle.card.score.info {} (TacticalCardDataManager.lua:71-74)
//     answers score and rewardedList, the claimed battle_card_box_reward row ids
//     (BattleCardScoreInfoMessage.lua:15-16; TacticalCardDataManager.lua:911-924). A row of the
//     current season with point <= score that is not in rewardedList is claimable
//     (HasClaimableBoxPointReward, :978-991; CardBoxRewardEntrance.lua:112-140), and the entrance
//     sends battle.card.score.reward {} for all of them (BattleCardScoreRewardMessage.lua:5-7).
//   - tactical-card-collection: init cardCollects[{cfgId, cardType, rewardStr "index;state|..."}]
//     holds each card type's stages, state 0 unavailable, 1 available, 2 received
//     (TacticalCardDataManager.lua:88-135, called from InitMessage.lua:489 before the open check).
//     The activated cards of a type are the ids in battle.card.collect.list's collectCards[{cardType,
//     cards "id;id;..."}] (:156-176, 240-244; GetBattleCardBookDataMessage.lua:14). A stage is claimed
//     when its state is 1, or 0 with battle_card_reward[cfgId].card_num[index+1] <= the activated
//     count (UITCCardBookView.lua:220-226; the red dot StageRewardItem.lua:106-112 shows a reached
//     stage sits at state 0 until claimed). The claim is battle.card.collect.reward {index: Int,
//     cardType: Int, cfgId: Int}, index being the number before the ';' (UITCCardBookView.lua:272;
//     StageRewardItem.lua:42-44, 75-76; ClaimBattleCardStageRewardMessage.lua:5-9), and its reply
//     carries the updated collectObj.
//
// Never open.battle.card.box, battle.card.choose.box, upgrades, resets or decompose.
const (
	tacticalBoxPointsFeature   = "tactical-card-box-points"
	tacticalCollectionFeature  = "tactical-card-collection"
	tacticalScoreInfoCmd       = "battle.card.score.info"
	tacticalScoreRewardCmd     = "battle.card.score.reward"
	tacticalCollectListCmd     = "battle.card.collect.list"
	tacticalCollectRewardCmd   = "battle.card.collect.reward"
	tacticalOpenSeason         = 4 // battle_card_param.k6 "4;2", table item 40413
	tacticalOpenSeasonDay      = 2
	tacticalStageUnavailable   = 0
	tacticalStageAvailable     = 1 // state 2 (received) is never claimed
	tacticalSeasonDayMs        = int64(24 * time.Hour / time.Millisecond)
	tacticalStateUnknownCfgLog = "cfgId not in battle_card_reward " + tableVersion + "; only state-1 stages are claimed"
)

// tacticalBoxRewards is table battle_card_box_reward 40413: row id -> season and point.
var tacticalBoxRewards = map[int64]struct{ season, point int64 }{
	401: {4, 1000}, 402: {4, 2000}, 403: {4, 3000}, 404: {4, 4000}, 405: {4, 5000},
	501: {5, 1000}, 502: {5, 2000}, 503: {5, 3000}, 504: {5, 4000}, 505: {5, 5000},
	601: {6, 1000}, 602: {6, 2000}, 603: {6, 3000}, 604: {6, 4000}, 605: {6, 5000},
	701: {7, 1000}, 702: {7, 2000}, 703: {7, 3000}, 704: {7, 4000}, 705: {7, 5000},
}

// tacticalCardNum is table battle_card_reward 40413: cfgId -> card_num, the activated cards each
// stage (0-based index) needs.
var tacticalCardNum = map[int64][]int64{
	40101: {2, 4, 6, 8, 9}, 40201: {3, 6, 9, 12, 16}, 40301: {1, 3, 5, 7, 9},
	50101: {3, 5, 7, 9, 11}, 50201: {4, 8, 12, 16, 20}, 50301: {1, 3, 5, 7, 9},
	60101: {3, 5, 7, 9, 11}, 60201: {4, 8, 13, 18, 22}, 60301: {1, 3, 5, 7, 9},
	70101: {3, 5, 7, 9, 11}, 70201: {4, 8, 13, 18, 22}, 70301: {1, 3, 5, 7, 9},
}

// tacticalStagePair is UpdateCardCollections' string.match(reward, "(%d+);(%d+)").
var tacticalStagePair = regexp.MustCompile(`(\d+);(\d+)`)

func init() {
	registerFeature(Feature{
		Name:      tacticalBoxPointsFeature,
		Summary:   "OPT-IN: claim the tactical card box-point chests the season's score has reached (static-only, not validated live)",
		DefaultOn: false,
		Run:       runTacticalBoxPoints,
	})
	registerFeature(Feature{
		Name:      tacticalCollectionFeature,
		Summary:   "OPT-IN: claim tactical card collection stage rewards reached by the activated cards (static-only, not validated live)",
		DefaultOn: false,
		Run:       runTacticalCollection,
	})
}

// tacticalCardsOpen is TacticalCardUtil.IsFunctionOpen; it returns the season and the reason when
// closed.
func tacticalCardsOpen(in *Init) (season int64, why string) {
	p := in.Object("playerServerSeasonInfo")
	season, _ = evNum(p, "seasonId")
	switch {
	case season > tacticalOpenSeason:
		return season, ""
	case season < tacticalOpenSeason:
		return season, "season " + strconv.FormatInt(season, 10) + " is before the tactical cards open (season 4 day 2)"
	}
	start, ok := evNum(p, "seasonStartTime")
	if !ok || start <= 0 {
		return season, "season 4 without a seasonStartTime"
	}
	if day := (evNow().UnixMilli()-start)/tacticalSeasonDayMs + 1; day < tacticalOpenSeasonDay {
		return season, "season 4 day " + strconv.FormatInt(day, 10) + " is before day 2"
	}
	return season, ""
}

func runTacticalBoxPoints(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	season, why := tacticalCardsOpen(in)
	if why != "" {
		slog.Info(tacticalBoxPointsFeature+": closed; nothing to claim", "reason", why)
		return nil
	}
	b := &evBatch{conn: conn}
	r := b.send(tacticalBoxPointsFeature+": score info", tacticalScoreInfoCmd, sfs.NewSFSObject())
	if r == nil {
		return b.err()
	}
	score, _ := evNum(r.Params, "score")
	due := tacticalBoxesDue(season, score, evNums(r.Params, "rewardedList"))
	slog.Info(tacticalBoxPointsFeature+": score info", "season", season, "score", score,
		"rewardedList", evNums(r.Params, "rewardedList"), "due", due)
	if len(due) > 0 {
		evTaskSend(b, tacticalBoxPointsFeature+": claim", tacticalScoreRewardCmd, sfs.NewSFSObject(), "due", due)
	}
	return b.err()
}

// tacticalBoxesDue lists the season's box rows with point <= score that rewarded lacks.
func tacticalBoxesDue(season, score int64, rewarded []int64) []int64 {
	got := map[int64]bool{}
	for _, id := range rewarded {
		got[id] = true
	}
	var due []int64
	for id := range tacticalBoxRewards {
		r := tacticalBoxRewards[id]
		if r.season == season && r.point <= score && !got[id] {
			due = append(due, id)
		}
	}
	slices.Sort(due)
	return due
}

// tacticalStage is one claimable collection stage.
type tacticalStage struct {
	cfgID, cardType, index, state int64
}

func runTacticalCollection(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if _, why := tacticalCardsOpen(in); why != "" {
		slog.Info(tacticalCollectionFeature+": closed; nothing to claim", "reason", why)
		return nil
	}
	collects := claimObjectsOrMap(in.Raw, "cardCollects")
	if len(collects) == 0 {
		slog.Info(tacticalCollectionFeature + ": init has no cardCollects; nothing to claim")
		return nil
	}
	b := &evBatch{conn: conn}
	r := b.send(tacticalCollectionFeature+": card book", tacticalCollectListCmd, sfs.NewSFSObject())
	if r == nil {
		return b.err()
	}
	active := tacticalActiveCards(r.Params)
	due := tacticalStagesDue(collects, active)
	slog.Info(tacticalCollectionFeature+": card book", "activeByType", active, "collections", len(collects), "due", len(due))
	for _, st := range due {
		p := sfs.NewSFSObject()
		p.PutInt("index", int32(st.index))
		p.PutInt("cardType", int32(st.cardType))
		p.PutInt("cfgId", int32(st.cfgID))
		if !evTaskSend(b, tacticalCollectionFeature+": stage", tacticalCollectRewardCmd, p,
			"cfgId", st.cfgID, "cardType", st.cardType, "index", st.index, "state", st.state) {
			break // the first refusal stops the claims
		}
	}
	return b.err()
}

// tacticalActiveCards counts the activated card ids per card type in a collect.list reply. Like
// UpdateCardBookData it merges every collectCards entry of a type into one set keyed by the
// numeric card id (cardBookData[cardType][tonumber(card)], TacticalCardDataManager.lua:156-176),
// so a card listed twice, in one entry or in two entries of the same type, counts once.
func tacticalActiveCards(reply *sfs.SFSObject) map[int64]int64 {
	seen := map[int64]map[int64]bool{}
	for _, c := range evObjects(reply, "collectCards") {
		typ, ok := evNum(c, "cardType")
		if !ok {
			continue
		}
		if seen[typ] == nil {
			seen[typ] = map[int64]bool{}
		}
		for _, id := range strings.Split(evString(c, "cards"), ";") {
			if n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err == nil {
				seen[typ][n] = true
			}
		}
	}
	out := map[int64]int64{}
	for typ, ids := range seen {
		out[typ] = int64(len(ids))
	}
	return out
}

// tacticalStagesDue lists the stages of the init collections the card book view would claim. The
// client keeps one collection per card type, the last entry of that type
// (cardCollections[cardType] = ..., TacticalCardDataManager.lua:101-119), so an earlier entry of a
// repeated type is not claimed.
func tacticalStagesDue(collects []*sfs.SFSObject, active map[int64]int64) []tacticalStage {
	last := map[int64]int{}
	for i, c := range collects {
		if typ, ok := evNum(c, "cardType"); ok {
			last[typ] = i
		}
	}
	var due []tacticalStage
	for i, c := range collects {
		cfg, ok1 := evNum(c, "cfgId")
		typ, ok2 := evNum(c, "cardType")
		if !ok1 || !ok2 || last[typ] != i {
			continue
		}
		need, known := tacticalCardNum[cfg]
		if !known {
			slog.Info(tacticalCollectionFeature+": "+tacticalStateUnknownCfgLog, "cfgId", cfg, "cardType", typ)
		}
		for _, seg := range strings.Split(evString(c, "rewardStr"), "|") {
			m := tacticalStagePair.FindStringSubmatch(seg)
			if m == nil {
				continue
			}
			idx, err1 := strconv.ParseInt(m[1], 10, 64)
			state, err2 := strconv.ParseInt(m[2], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			reached := state == tacticalStageUnavailable && known && idx < int64(len(need)) && need[idx] <= active[typ]
			if state == tacticalStageAvailable || reached {
				due = append(due, tacticalStage{cfgID: cfg, cardType: typ, index: idx, state: state})
			}
		}
	}
	return due
}
