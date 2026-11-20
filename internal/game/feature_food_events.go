package game

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Cooking score boxes, Banquet level boxes and Banquet v2 claims (MASTER.md G15; aud-events F9,
// cov-ui-city F4 and their verdicts), OPT-IN, claims only. Static-only, from the 1.0.370 Lua, not
// sent live yet. MASTER asks for a log-only first run, since no reply of these events has been
// seen: each feature has a -plan twin that sends only the info read and logs the state it parsed
// and every claim it would send.
//
//   - food-event-boxes: Cooking (type 150) and Banquet (type 151), from the info replies
//     event-shop-freebies already reads (activity.mak.food.info / activity.food.party.info {aid:
//     Int}); the client parses only info[0] (ActivityMakeFoodInfoMessage.lua:13-17,
//     ActivityFoodPartyInfoMessage.lua:13-17).
//     Cooking: info[0] carries id, totalScore, score_reward[{score, reward}] and rewards, a CSV of
//     the claimed scores (ActCookingData.lua:23-49, GetActScoreList :66-77). A box with state 0 and
//     its score reached sends activity.mak.food.score.reward {aid: Int, id: Int info[0].id, score:
//     Int} (CookingRewardBoxItem.lua:57-65; ActivityMakeFoodScoreRewardMessage.lua:5-12).
//     Banquet: info[0] carries id, level, level_reward[] and levels, a CSV of the claimed levels
//     (ActBanquetData.lua:44-74). Its view is UIActChristmasTree (ActivityContentHandler,
//     EnumType.lua:13981-13984): box i (level i) with i not claimed and level >= i sends
//     activity.food.party.level.reward {aid: Int, id: Int info[0].id, level: Int i}
//     (UIActChristmasTree.lua:467-477; ActivityFoodPartyLevelRewardMessage.lua:5-12; the red dot
//     counts the same over level_reward, GetCanGetScoreBoxCount :347-355).
//   - banquet-v2-claims: Banquet v2 (BanquetAttackMonster, type 160, EnumType.lua:6419). The client
//     sends activity.food.party.v2.info {aid: Int} for the open activity (ActivityListDataManager.
//     lua:413-418; ActivityFoodPartyV2InfoMessage.lua:5-19): info[0] carries id, level,
//     level_reward[], levels (claimed CSV) and extraReward; taskArr[{taskId, state}] is at the top
//     level (ActBanquetV2Data.lua:69-141). (1) Each task in state 1 (TaskState.CanReceive) sends
//     activity.food.party.v2.task.reward {activityId: Int, taskId: UtfString}
//     (BanquetAttackMonsterTaskItem.lua:144-168; ActivityFoodPartyV2TaskRewardMessage.lua:4-8). (2)
//     Each level box i not claimed with level >= i sends activity.food.party.v2.level.reward {aid:
//     Int, id: Int info[0].id, level: Int i} (BanquetAttackMonsterLevelRewardItem.lua:102-110;
//     GetActScoreList :238-250). (3) A non-empty extraReward (the stash) sends
//     activity.food.party.v2.extra.reward {aid: Int} (UIBanquetFinRewardGetNewCtrl.lua:37-43;
//     ActivityFoodPartyV2ExtraRewardMessage.lua:5-8).
//
// The level boxes run from 1 to the reply's level, and no further than level_reward's length when
// the reply has one (the red-dot count's bound). Never the donate, attack.monster or monster.confirm
// commands; activity.food.party.v2.reward.free has no sender in the client. Each claim's reply is
// logged at Info and an errorCode stops that feature (no benign code is registered).
const (
	foodBoxesFeature         = "food-event-boxes"
	banquetV2Feature         = "banquet-v2-claims"
	foodPlanSuffix           = "-plan"
	banquetV2ActType   int32 = 160 // EnumActivity.BanquetAttackMonster
	cookingInfoCmd           = "activity.mak.food.info"
	banquetInfoCmd           = "activity.food.party.info"
	banquetV2InfoCmd         = "activity.food.party.v2.info"
	cookingScoreCmd          = "activity.mak.food.score.reward"
	banquetLevelCmd          = "activity.food.party.level.reward"
	banquetV2TaskCmd         = "activity.food.party.v2.task.reward"
	banquetV2LevelCmd        = "activity.food.party.v2.level.reward"
	banquetV2ExtraCmd        = "activity.food.party.v2.extra.reward"
	foodTaskCanReceive       = 1  // TaskState.CanReceive (EnumType.lua:5292-5298)
	foodMaxLevelClaims       = 60 // a bound on level boxes per activity and run
)

func init() {
	for _, dry := range []bool{false, true} {
		suffix, verb, note := "", "claim", ""
		if dry {
			suffix, verb, note = foodPlanSuffix, "log (read-only, no claim)", "; run this first"
		}
		registerFeature(Feature{
			Name:    foodBoxesFeature + suffix,
			Summary: fmt.Sprintf("OPT-IN: %s the Cooking score boxes and Banquet level boxes reached%s (static-only, not validated live)", verb, note),
			Run:     func(conn *session.GameConn, in *Init) error { return runFoodBoxes(conn, in, dry) },
		})
		registerFeature(Feature{
			Name:    banquetV2Feature + suffix,
			Summary: fmt.Sprintf("OPT-IN: %s Banquet v2's finished tasks, reached level boxes and stash%s (static-only, not validated live)", verb, note),
			Run:     func(conn *session.GameConn, in *Init) error { return runBanquetV2(conn, in, dry) },
		})
	}
}

// foodClaim is one claim a food event offers.
type foodClaim struct {
	what string
	cmd  string
	p    *sfs.SFSObject
}

// evCSVNums reads a list sent as a comma-separated string (string.split(s, ","), tonumber each) or,
// failing that, as a number array.
func evCSVNums(o *sfs.SFSObject, key string) map[int64]bool {
	out := map[int64]bool{}
	if v, ok := o.Get(key); ok {
		if s, ok := v.Val.(string); ok {
			for _, f := range strings.Split(s, ",") {
				if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
					out[n] = true
				}
			}
			return out
		}
	}
	for _, n := range evNums(o, key) {
		out[n] = true
	}
	return out
}

// cookingClaims are the Cooking score boxes due in info[0] (ActCookingData.lua:23-49, 66-77).
func cookingClaims(a Activity, info *sfs.SFSObject) []foodClaim {
	e := evFirstInfo(info)
	id, ok := evNum(e, "id")
	if !ok {
		return nil
	}
	total, _ := evNum(e, "totalScore")
	done := evCSVNums(e, "rewards")
	var out []foodClaim
	for _, r := range evObjects(e, "score_reward") {
		score, ok := evNum(r, "score")
		if !ok || total < score || done[score] {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		p.PutInt("id", int32(id))
		p.PutInt("score", int32(score))
		out = append(out, foodClaim{fmt.Sprintf("cooking box %d (activity %d, score %d)", score, a.ID, total), cookingScoreCmd, p})
	}
	return out
}

// foodLevelClaims are the level boxes 1..level not in levels, bounded by level_reward's length when
// the reply has one, for Banquet (v1, cmd banquetLevelCmd) and Banquet v2.
func foodLevelClaims(a Activity, e *sfs.SFSObject, cmd, label string) []foodClaim {
	id, ok := evNum(e, "id")
	if !ok {
		return nil
	}
	level, _ := evNum(e, "level")
	upper := level
	if n := int64(len(evObjects(e, "level_reward"))); n > 0 && n < upper {
		upper = n
	}
	upper = min(upper, foodMaxLevelClaims)
	done := evCSVNums(e, "levels")
	var out []foodClaim
	for lv := int64(1); lv <= upper; lv++ {
		if done[lv] {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		p.PutInt("id", int32(id))
		p.PutInt("level", int32(lv))
		out = append(out, foodClaim{fmt.Sprintf("%s level box %d (activity %d, level %d)", label, lv, a.ID, level), cmd, p})
	}
	return out
}

// banquetV2Claims are the v2 claims due: finished tasks, level boxes, then the stash.
func banquetV2Claims(a Activity, info *sfs.SFSObject) []foodClaim {
	var out []foodClaim
	for _, t := range evObjects(info, "taskArr") {
		id := evString(t, "taskId")
		if st, _ := evNum(t, "state"); st != foodTaskCanReceive || id == "" {
			continue
		}
		p := evActivityIDParam(a)
		p.PutUtfString("taskId", id)
		out = append(out, foodClaim{fmt.Sprintf("banquet v2 task %s (activity %d)", id, a.ID), banquetV2TaskCmd, p})
	}
	e := evFirstInfo(info)
	out = append(out, foodLevelClaims(a, e, banquetV2LevelCmd, "banquet v2")...)
	if evNonEmpty(e, "extraReward") {
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		out = append(out, foodClaim{fmt.Sprintf("banquet v2 stash (activity %d)", a.ID), banquetV2ExtraCmd, p})
	}
	return out
}

// foodRunClaims logs (dry) or sends the claims; a failed claim stops the rest.
func foodRunClaims(b *evBatch, feature string, dry bool, claims []foodClaim) bool {
	for _, c := range claims {
		if dry {
			slog.Info(feature+": would claim", "what", c.what, "cmd", c.cmd, "params", c.p.StringRedacted())
			continue
		}
		if !evOptInClaim(b, feature, c.what, c.cmd, c.p) {
			return false
		}
	}
	return true
}

func runFoodBoxes(conn *session.GameConn, in *Init, dry bool) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	feature := foodBoxesFeature
	if dry {
		feature += foodPlanSuffix
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, step := range []struct {
		typ   int32
		cmd   string
		label string
	}{{actTypeCooking, cookingInfoCmd, "cooking"}, {actTypeBanquet, banquetInfoCmd, "banquet"}} {
		fetch := evIntInfo(step.label+" info response", step.cmd, "aid")
		for _, a := range s.Open(step.typ) {
			if b.dead {
				return b.err()
			}
			info, err := fetch(s, a)
			if err != nil {
				b.add(err)
				continue
			}
			e := evFirstInfo(info)
			var claims []foodClaim
			if step.typ == actTypeCooking {
				claims = cookingClaims(a, info)
				total, _ := evNum(e, "totalScore")
				slog.Info(feature+": cooking", "activity", a.ID, "hasInfo", e != nil, "totalScore", total,
					"boxes", len(evObjects(e, "score_reward")), "claimed", evString(e, "rewards"), "due", len(claims))
			} else {
				claims = foodLevelClaims(a, e, banquetLevelCmd, "banquet")
				level, _ := evNum(e, "level")
				slog.Info(feature+": banquet", "activity", a.ID, "hasInfo", e != nil, "level", level,
					"levelRewards", len(evObjects(e, "level_reward")), "claimed", evString(e, "levels"), "due", len(claims))
			}
			if !foodRunClaims(b, feature, dry, claims) {
				return b.err()
			}
			if len(claims) > 0 && !dry {
				s.Forget(step.cmd, a)
			}
		}
	}
	return b.err()
}

func runBanquetV2(conn *session.GameConn, in *Init, dry bool) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	feature := banquetV2Feature
	if dry {
		feature += foodPlanSuffix
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	fetch := evIntInfo(feature+": info", banquetV2InfoCmd, "aid")
	for _, a := range s.Open(banquetV2ActType) {
		if b.dead {
			break
		}
		info, err := fetch(s, a)
		if err != nil {
			b.add(err)
			break // a read not validated live: an error stops the feature
		}
		e := evFirstInfo(info)
		claims := banquetV2Claims(a, info)
		level, _ := evNum(e, "level")
		slog.Info(feature+": info", "activity", a.ID, "hasInfo", e != nil, "level", level,
			"levelRewards", len(evObjects(e, "level_reward")), "claimed", evString(e, "levels"),
			"tasks", len(evObjects(info, "taskArr")), "extraReward", evNonEmpty(e, "extraReward"), "due", len(claims))
		if !foodRunClaims(b, feature, dry, claims) {
			break
		}
		if len(claims) > 0 && !dry {
			s.Forget(banquetV2InfoCmd, a)
		}
	}
	return b.err()
}
