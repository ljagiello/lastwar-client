package game

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Treasure Hunt (MASTER.md §4 #68), opt-in and static-only. It covers the 12-card "Bullseye Loot"
// event, where each card costs one owned pickaxe, in both client versions:
//
//   - v1, activity type 130 (ActivityListDataManager.lua:368-372 -> DigActivityManager): info
//     get.dig.activity.info {activityId: Int}, big-reward choice choose.dig.big.reward
//     {activityId, bigRewardIndex: Int} and dig start.activity.dig {activityId, digIndex: Int}
//     (GetDigActivityInfoMessage.lua:4-7, SelectFinalDigRewardMessage.lua:4-8,
//     DigOneBlockMessage.lua:4-8). Rewards land with each dig; v1 has nothing else to claim.
//   - v2, type 139 (:575-579 -> ActivityTreasureHuntNewManager): the .v2 forms of the same three
//     (ActivityTreasureHuntNewGetInfoMessage.lua:4-7, ...SelectFinalDigReward.lua,
//     ...DigOneBlockMessage.lua:4-8). It adds two claims, both {aid: Int}:
//     dig.receive.big.reward for a progress tier and dig.extra.reward for the stored rewards
//     (...ClaimBigRewardMessage.lua:3-6, ...ClaimStoredRewardMessage.lua:3-6).
//
// The info reply (DigActivityData.lua:29-75, ActivityTreasureHuntNewData.lua:43-116) carries
// these fields: level, the levels finished; bigRewardIndex, 0 until a grand prize is chosen;
// digRecord[{digIndex, goodsIndex}], the cards dug this level, where goodsIndex 0 is the grand
// prize. v2 adds bigRewardReceive (the tier levels claimed), totalReward (the stored rewards) and
// primaryPrize ("level;item;count|...").
// A reply without activityId is no data (UpdateDigInfo returns on it, DigActivityManager.lua:25-28).
//
// Per open activity (CheckIsSend), the run does this, in the client's order:
//
//  1. v2 progress tiers: a tier is claimable once its level <= level+1 and it is not in
//     bigRewardReceive (GetBigRewardClaimStateByLevel, ActivityTreasureHuntNewManager.lua:477-494).
//     Each dig.receive.big.reward claims one tier; the client re-reads the info and repeats while
//     one is due, stopping on a reply with no reward (:510-551). That also clears the multi-round
//     block on digging (ActivityTreasureHuntNewMainComponent.lua:801-833).
//  2. While pickaxes are owned and the level is below the last (finishedLv >= #activity_dig_para
//     rows is MaxLevelFin, ...MainComponent.lua:974-978), and for v2 below stop_level, where the
//     client's auto-dig stops (:941-948): choose the grand prize if bigRewardIndex is 0, then dig
//     the undug cards 1..12 one at a time. The card count is cardNum (TreasureHuntActivityMain.lua:32,
//     ...MainComponent.lua:41). A goodsIndex 0 result ends the level, so the run re-reads the info
//     and carries on.
//  3. v2 stored rewards: dig.extra.reward when totalReward is non-empty (the Claim button,
//     UITreasureHuntNewHistoryView.lua:233-241).
//
// The grand-prize choice is a player decision. This sends index 1, which the select window marks
// recommended (beRecommand on index == 1, UITreasureHuntBigRewardSelectItem.lua:52 and
// UITreasureHuntNewBigRewardSelectItem.lua:52); it never sends recommend.select{,.v2}, the
// "Set as default" toggle.
//
// What a dig costs: one pickaxe, item activity_dig.cost_goods (620002 in every row, table
// 39432). The client digs only while ItemData:GetItemCount(pickaxe) > 0
// (TreasureHuntActivityMain.lua:412-420, 518-526; ...MainComponent.lua:1015-1025, 1164-1175).
// goods_free is 0 in every row, so no dig is ever free. The run counts the pickaxes in init items
// and decrements the count per dig. It stops on 120021 ("Not enough items.", registered benign).
// It never buys: buy.activity.dig[.v2].goods costs 100 diamonds a pickaxe, and
// digsCheckRequest refuses it.
//
// The client's auto-dig is a loop of single digs over the undug cards in random order
// (TreasureHuntActivityMain.lua:465-531, ...MainComponent.lua:1099-1180). This digs them in index
// order: the card contents come from the server, so the order changes nothing. Not sent:
// start.activity.batch.dig.v2, which v2 uses when activity_dig.animation_type is 1
// (...Manager.lua:608-627), because it digs an unbounded number of cards in one call and the
// pickaxe count and the action cap could not hold; and auto.activity.dig{,.v2}, which no UI calls
// in 1.0.364.
//
// treasure-hunt-plan sends only the two info requests and logs every choice, dig and claim it
// would make for the current level. It cannot see past the next grand-prize card.
const (
	treasureHuntInfoCmd     = "get.dig.activity.info"
	treasureHuntChooseCmd   = "choose.dig.big.reward"
	treasureHuntDigCmd      = "start.activity.dig"
	treasureHuntInfoV2Cmd   = "get.dig.v2.activity.info"
	treasureHuntChooseV2Cmd = "choose.dig.v2.big.reward"
	treasureHuntDigV2Cmd    = "start.activity.dig.v2"
	treasureHuntTierCmd     = "dig.receive.big.reward"
	treasureHuntStoredCmd   = "dig.extra.reward"

	treasureHuntCards       = 12
	treasureHuntRecommended = 1
	treasureHuntV2SubType   = 5 // every type-139 row's tableInfoType
)

// treasureHuntDig is one activity_dig row (table 39432), keyed by id. The activity's
// tableInfoType (its subType, ActivityInfoData.lua:225) picks the row (DigActivityManager.lua:
// 157-167; ActivityTreasureHuntNewManager.lua:157-171, 234-240).
type treasureHuntDig struct {
	pickaxe   string  // cost_goods
	maxLevel  int64   // activity_dig_para rows with dig_id = id
	stopLevel int64   // stop_level
	tiers     []int64 // primary_prize levels, used when the info reply has no primaryPrize
}

var treasureHuntDigs = map[int64]treasureHuntDig{
	1: {pickaxe: "620002", maxLevel: 125, stopLevel: 100},
	2: {pickaxe: "620002", maxLevel: 50, stopLevel: 100},
	3: {pickaxe: "620002", maxLevel: 100, stopLevel: 100},
	4: {pickaxe: "620002", maxLevel: 100, stopLevel: 100},
	5: {pickaxe: "620002", maxLevel: 100, stopLevel: 100, tiers: []int64{5, 10, 20, 30, 40, 60, 80, 100}},
	6: {pickaxe: "620002", maxLevel: 100, stopLevel: 100, tiers: []int64{
		5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85, 90, 95, 100,
	}},
}

// treasureHuntV1SubTypes is activity.tableInfoType for the type-130 ids (table 39432).
var treasureHuntV1SubTypes = map[int32]int64{
	3009: 1, 5104: 1, 6004: 3, 6006: 4, 14003: 2, 94304: 4, 1000228: 4, 1000229: 4, 1000230: 4,
	1000274: 4, 1000275: 4, 8130001: 4, 8130002: 4, 8130003: 4,
}

func init() {
	registerFeature(Feature{
		Name: "treasure-hunt",
		Summary: "OPT-IN, SPENDS OWNED PICKAXES: Treasure Hunt v1/v2: pick the recommended grand prize, dig cards with " +
			"owned pickaxes only (never buys, never diamonds), claim v2 progress tiers and stored rewards; static-only",
		Run: runTreasureHunt,
	})
	registerFeature(Feature{
		Name:    "treasure-hunt-plan",
		Summary: "read-only: log the grand-prize choice, digs and claims treasure-hunt would send; sends only the info requests",
		Run:     runTreasureHuntPlan,
	})
	session.RegisterBenignErrorCode(digsNoItemsCode, treasureHuntDigCmd, treasureHuntDigV2Cmd)
}

func runTreasureHunt(conn *session.GameConn, in *Init) error { return treasureHuntRun(conn, in, false) }

func runTreasureHuntPlan(conn *session.GameConn, in *Init) error {
	return treasureHuntRun(conn, in, true)
}

// treasureHunt is one run's state: the sender and the pickaxes left per item id.
type treasureHunt struct {
	r     *digsRun
	owned map[string]int64
}

func treasureHuntRun(conn *session.GameConn, in *Init, dry bool) error {
	feature := "treasure-hunt"
	if dry {
		feature += "-plan"
	}
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	acts := Activities(conn, in).Open(actTypeTreasureHunt, actTypeTreasureHuntNew)
	if len(acts) == 0 {
		slog.Info(feature + ": no Treasure Hunt event open")
		return nil
	}
	th := &treasureHunt{r: newDigsRun(conn, in, feature, dry), owned: duelInitItemCounts(in)}
	for _, a := range acts {
		if th.r.halt {
			break
		}
		th.run(a)
	}
	return th.r.err()
}

// treasureHuntCmds is the command set of one version.
type treasureHuntCmds struct{ info, choose, dig string }

func (th *treasureHunt) cmds(v2 bool) treasureHuntCmds {
	if v2 {
		return treasureHuntCmds{treasureHuntInfoV2Cmd, treasureHuntChooseV2Cmd, treasureHuntDigV2Cmd}
	}
	return treasureHuntCmds{treasureHuntInfoCmd, treasureHuntChooseCmd, treasureHuntDigCmd}
}

// treasureHuntState is the part of an info reply the run acts on.
type treasureHuntState struct {
	level     int64 // levels finished; the current level is level+1
	bigReward int64 // bigRewardIndex
	dug       map[int64]bool
}

func treasureHuntParse(info *sfs.SFSObject) treasureHuntState {
	st := treasureHuntState{dug: map[int64]bool{}}
	st.level, _ = claimInt(info, "level")
	st.bigReward, _ = claimInt(info, "bigRewardIndex")
	for _, rec := range claimObjectsOrMap(info, "digRecord") {
		if idx, ok := claimInt(rec, "digIndex"); ok {
			st.dug[idx] = true
		}
	}
	return st
}

func (th *treasureHunt) run(a Activity) {
	v2 := a.Type == actTypeTreasureHuntNew
	sub, ok := treasureHuntV1SubTypes[a.ID]
	if v2 {
		sub, ok = treasureHuntV2SubType, true
	}
	dig, known := treasureHuntDigs[sub]
	if !ok || !known {
		slog.Info(th.r.feature+": activity has no known activity_dig row; skipping", "activity", a.ID)
		return
	}
	c := th.cmds(v2)
	info := th.info(a, c)
	if info == nil {
		return
	}
	if v2 {
		info = th.claimTiers(a, dig, c, info)
	}
	dugTotal := 0
	for info != nil && !th.r.halt {
		st := treasureHuntParse(info)
		slog.Info(th.r.feature+": level", "activity", a.ID, "v2", v2, "level", st.level+1, "bigRewardIndex", st.bigReward,
			"cardsDug", len(st.dug), "pickaxes", th.owned[dig.pickaxe])
		if st.level >= dig.maxLevel {
			slog.Info(th.r.feature+": every level is finished", "activity", a.ID, "levels", dig.maxLevel)
			break
		}
		if v2 && dig.stopLevel > 0 && st.level+1 >= dig.stopLevel {
			slog.Info(th.r.feature+": at the stop level, where the client's auto-dig stops", "activity", a.ID, "stopLevel", dig.stopLevel)
			break
		}
		if th.owned[dig.pickaxe] <= 0 {
			slog.Info(th.r.feature+": no pickaxes owned; nothing to dig", "activity", a.ID, "item", dig.pickaxe)
			break
		}
		if st.bigReward == 0 {
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			p.PutInt("bigRewardIndex", treasureHuntRecommended)
			label := fmt.Sprintf("treasure hunt %d level %d grand prize (recommended choice %d)", a.ID, st.level+1, treasureHuntRecommended)
			if _, res := th.r.write(label, c.choose, p); res != digsDone {
				break
			}
		}
		n, next := th.digLevel(a, c, dig, st)
		dugTotal += n
		if !next {
			break
		}
		info = th.info(a, c)
		if v2 && info != nil {
			info = th.claimTiers(a, dig, c, info)
		}
	}
	if v2 && !th.r.halt {
		if dugTotal > 0 && !th.r.dry {
			info = th.info(a, c)
		}
		th.claimStored(a, info)
	}
	slog.Info(th.r.feature+": done", "activity", a.ID, "cardsDug", dugTotal, "pickaxesLeft", th.owned[dig.pickaxe])
}

// info sends the version's info request; a reply without activityId is logged as no data.
func (th *treasureHunt) info(a Activity, c treasureHuntCmds) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutInt("activityId", a.ID)
	reply := th.r.read(fmt.Sprintf("treasure hunt %d info", a.ID), c.info, p)
	if reply == nil {
		return nil
	}
	if !reply.Has("activityId") {
		slog.Info(th.r.feature+": info reply carries no dig data; treating as nothing to do", "activity", a.ID,
			"reply", reply.StringRedacted())
		return nil
	}
	return reply
}

// digLevel digs the current level's undug cards with owned pickaxes. next is true when the level
// ended (the grand prize came out, or every card is dug) and the info should be read again.
func (th *treasureHunt) digLevel(a Activity, c treasureHuntCmds, dig treasureHuntDig, st treasureHuntState) (n int, next bool) {
	for idx := int64(1); idx <= treasureHuntCards; idx++ {
		if st.dug[idx] {
			continue
		}
		if th.owned[dig.pickaxe] <= 0 {
			slog.Info(th.r.feature+": out of pickaxes", "activity", a.ID, "level", st.level+1)
			return n, false
		}
		p := sfs.NewSFSObject()
		p.PutInt("activityId", a.ID)
		p.PutInt("digIndex", int32(idx))
		reply, res := th.r.write(fmt.Sprintf("treasure hunt %d level %d card %d", a.ID, st.level+1, idx), c.dig, p)
		switch res {
		case digsDone:
		case digsRefused:
			if evString(reply, "errorCode") == digsNoItemsCode {
				slog.Info(th.r.feature+": the server reports no pickaxes left", "activity", a.ID)
				th.owned[dig.pickaxe] = 0
			}
			return n, false
		default:
			return n, false
		}
		th.owned[dig.pickaxe]--
		n++
		if th.r.dry {
			continue
		}
		if g, ok := claimInt(reply, "goodsIndex"); ok && g == 0 {
			slog.Info(th.r.feature+": grand prize dug; level complete", "activity", a.ID, "level", st.level+1)
			return n, true
		}
	}
	return n, !th.r.dry
}

// treasureHuntTiersDue lists the v2 progress tiers claimable now.
func treasureHuntTiersDue(info *sfs.SFSObject, dig treasureHuntDig) []int64 {
	tiers := treasureHuntPrimaryPrize(evString(info, "primaryPrize"))
	if len(tiers) == 0 {
		tiers = dig.tiers
	}
	level, _ := claimInt(info, "level")
	claimed := claimInts(info, "bigRewardReceive")
	var due []int64
	for _, t := range tiers {
		if t <= level+1 && !slices.Contains(claimed, t) {
			due = append(due, t)
		}
	}
	return due
}

// treasureHuntPrimaryPrize reads the tier levels from "level;item;count|..."
// (ParsePrimaryPrizeList, ActivityTreasureHuntNewManager.lua:739-759).
func treasureHuntPrimaryPrize(s string) []int64 {
	var out []int64
	for part := range strings.SplitSeq(s, "|") {
		f := strings.Split(part, ";")
		if len(f) < 3 {
			continue
		}
		if lv, err := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64); err == nil {
			out = append(out, lv)
		}
	}
	slices.Sort(out)
	return out
}

// claimTiers claims the due v2 progress tiers one call at a time, re-reading the info after each,
// and returns the latest info.
func (th *treasureHunt) claimTiers(a Activity, dig treasureHuntDig, c treasureHuntCmds, info *sfs.SFSObject) *sfs.SFSObject {
	limit := max(len(dig.tiers), len(treasureHuntPrimaryPrize(evString(info, "primaryPrize"))))
	for i := 0; info != nil && !th.r.halt; i++ {
		due := treasureHuntTiersDue(info, dig)
		if len(due) == 0 {
			return info
		}
		if i >= limit {
			slog.Warn(th.r.feature+": progress tiers still due after one claim per tier; leaving them", "activity", a.ID, "due", due)
			return info
		}
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		reply, res := th.r.write(fmt.Sprintf("treasure hunt %d progress reward (tiers due %v)", a.ID, due), treasureHuntTierCmd, p)
		if th.r.dry || res != digsDone {
			return info
		}
		if !treasureHuntHasItems(reply, "reward") {
			slog.Info(th.r.feature+": progress claim returned no reward; stopping the tier claims", "activity", a.ID)
			return info
		}
		info = th.info(a, c)
	}
	return info
}

// claimStored claims the v2 stored rewards when the info lists any.
func (th *treasureHunt) claimStored(a Activity, info *sfs.SFSObject) {
	if info == nil || !treasureHuntHasItems(info, "totalReward") {
		return
	}
	p := sfs.NewSFSObject()
	p.PutInt("aid", a.ID)
	th.r.write(fmt.Sprintf("treasure hunt %d stored rewards", a.ID), treasureHuntStoredCmd, p)
}

// treasureHuntHasItems reports whether key holds a non-empty array or object (the Lua's
// not table.IsNullOrEmpty).
func treasureHuntHasItems(o *sfs.SFSObject, key string) bool {
	v, ok := o.Get(key)
	if !ok {
		return false
	}
	switch c := v.Val.(type) {
	case *sfs.SFSArray:
		return c != nil && len(c.Items()) > 0
	case *sfs.SFSObject:
		return c != nil && len(c.Keys()) > 0
	}
	return false
}
