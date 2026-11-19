package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Event reward bubbles over bases (MASTER G7, cov-scene-chat/F3), opt-in and static-only. During
// some events a status on a player's HQ shows a reward bubble on the world map; a tap claims it.
// The HQ tiles come from this run's alliance tile scan (sharedAllianceTileScan), whose BuildInfo
// carries the statuses (field 32, worldpoint_proto.go). Two bubbles are claimed:
//
// Thanksgiving (ActGiftGiving, activity type 321), up to 10 a day on other players' bases. The
// client shows a bubble for a status of lw_status type2 31 (StatusType2.BuildingRewardBubble1,
// EnumType.lua:15232) that has not expired, on an HQ (CityDomeProtectEffectManager.lua:932, the
// FUN_BUILD_MAIN branch) whose owner is not the player, whose point is on its owner's home server
// and the player's (serverId == srcServerId == the source server), and whose (owner, status) pair is
// not in the player's receive dict (:1147-1165). For these statuses lw_status.activity_id is empty,
// so ANY listed pair counts as received, expired or not (ActivityReceiveDataManager.lua:103-123;
// the receive dict comes from init activityReceiveArr, strings "<ownerUid>_<statusId>_<expire s>",
// :125-141). The bubble hides once the day's count reaches activity_thanksgiving.day_receive_num
// (10 in both 40413 rows), the count being thanksgiving_times[the open activity] while now is
// before thanksgiving_times_expire (ActGiftGivingDataManager.lua:199-261, from init and from every
// claim reply: an array of maps {"<activityId>": n}). The tap sends
// `thanksgiving.receive.base.reward {activityId: Int, otherUid: UtfString, statusId: Int}`
// (BuildingRewardBubbleEffect.lua:211-216, ThanksgivingReceiveBaseRewardMessage.lua:4-9), the
// activity being the open type-321 one (GetOneOpenActId). Go reads only alliance members' HQs,
// the bases the scan reads, a subset of the client's. Never the type2-33 bubble: its tap is a
// scout march (:217-218).
//
// Valentine (ActValentineReceiveGift, type 332), the player's own base only. A status of type2 35
// (BuildingRewardBubble3) on the player's own HQ, not expired, whose (own uid, status) pair is not
// in the receive dict unexpired (this activity has a daily cap, bubble_times 7, so the dict's
// expire time counts), and while fewer than bubble_times unexpired pairs of that status are listed
// (GetCurNumByActId), is claimed with `valentine.owner.base.reward {activityId: Int, statusId: Int}`,
// the activity being lw_status.activity_id (BuildingGetRewardBubbleEffect.lua:130-140, 211-218;
// ValentineOwnerBaseRewardMessage.lua:4-8). Never valentine.cross.base.reward (away from home) or
// the scout march of other bases.
//
// Not done: the music-festival party bubble (its claim count is untraced and the bubble turns into
// a share-to-chat button). Unverified live: whether world.get.block returns field 32 to a session
// that never entered the world scene, the "already received" errorCode, and whether the owner sees
// the claim. Every claim is logged at Info with the reply's keys, and the first errorCode of any
// kind stops the run.
const (
	baseBubblesFeature        = "base-bubbles"
	thanksgivingClaimCmd      = "thanksgiving.receive.base.reward"
	valentineOwnerClaimCmd    = "valentine.owner.base.reward"
	thanksgivingActivityType  = 321 // EnumActivity.ActGiftGiving (EnumType.lua:6409)
	valentineActivityType     = 332 // EnumActivity.ActValentineReceiveGift (EnumType.lua:6474)
	thanksgivingDayReceiveNum = 10  // activity_thanksgiving.day_receive_num, rows 1 and 2 of tables 40413
	baseBubblesMaxClaims      = thanksgivingDayReceiveNum
)

// thanksgivingActivities are table activity's type-321 rows (tables 40413): 99108 (2024) and
// 99302 (2026, data_date 2026-11-22; the generated activity table of this tree is older and lacks
// it, so the type is taken from here).
var thanksgivingActivities = []int32{99108, 99302}

// thanksgivingStatuses are lw_status's type2-31 rows (tables 40413; activity_id empty, time 86400).
var thanksgivingStatuses = map[int32]bool{500521: true, 500522: true, 500550: true, 500551: true}

// valentineStatus is a lw_status type2-35 row: its activity_id and that activity's
// activity_Valentine_Get bubble_times (day_first_status is the status itself).
type valentineStatus struct {
	activityID int32
	dayCap     int
}

// valentineStatuses are lw_status's type2-35 rows (tables 40413).
var valentineStatuses = map[int32]valentineStatus{520526: {99137, 7}, 520533: {99192, 7}}

func init() {
	registerFeature(Feature{
		Name: baseBubblesFeature,
		Summary: "OPT-IN: claim the event reward bubbles over bases: Thanksgiving on alliance members' HQs (up to 10 a " +
			"day) and the Valentine bubble on your own HQ, found by the alliance tile scan; free, no march, the base's " +
			"owner may see the claim (static-only, not validated live)",
		Run: runBaseBubbles,
	})
}

// receivedPair is one init activityReceiveArr entry.
type receivedPair struct {
	owner    string
	statusID int32
}

// baseReceiveDict is the client's activityStatusReceiveDict: (owner, status) -> expire time (s).
type baseReceiveDict map[receivedPair]int64

// newBaseReceiveDict reads init activityReceiveArr as SetActivityStatusReceiveDictInitMsg does
// (ActivityReceiveDataManager.lua:125-141): each string splits on "_" into exactly three parts.
func newBaseReceiveDict(in *Init) baseReceiveDict {
	d := baseReceiveDict{}
	v, ok := in.Raw.Get("activityReceiveArr")
	if !ok {
		return d
	}
	var items []string
	switch a := v.Val.(type) {
	case []string:
		items = a
	case *sfs.SFSArray:
		for _, it := range a.Items() {
			if s, ok := it.Val.(string); ok {
				items = append(items, s)
			}
		}
	}
	for _, s := range items {
		parts := strings.Split(s, "_")
		if len(parts) != 3 {
			continue
		}
		id, _ := strconv.ParseInt(parts[1], 10, 32)
		exp, _ := strconv.ParseInt(parts[2], 10, 64)
		d[receivedPair{parts[0], int32(id)}] = exp
	}
	return d
}

// thanksgivingCount is the day's Thanksgiving count for activity act (GetRewardBubbleGetNum): the
// value under act in thanksgiving_times while now is not past thanksgiving_times_expire.
func thanksgivingCount(o *sfs.SFSObject, act int32, now time.Time) (int64, bool) {
	exp, ok := evNum(o, "thanksgiving_times_expire")
	if ok && now.After(evUnix(exp)) {
		return 0, true
	}
	key := strconv.Itoa(int(act))
	for _, m := range claimObjects(o, "thanksgiving_times") {
		if n, ok := evNum(m, key); ok {
			return n, true
		}
	}
	return 0, o.Has("thanksgiving_times")
}

// openActivityOf returns the first init activity among ids that CheckIsSend accepts, by id, with
// its type taken from typ (the generated table may lack the id).
func openActivityOf(in *Init, ids []int32, typ int32) (int32, bool) {
	entries, _ := initActivityEntries(in)
	now, hq := evNow().UnixMilli(), activityHQLevel(in)
	var open []int32
	for _, e := range entries {
		id, ok := activityEntryID(e.obj, e.key)
		if ok && slices.Contains(ids, id) && (Activity{ID: id, Type: typ, NeedHQ: activityNeedHQ(id), Raw: e.obj}).open(now, hq) {
			open = append(open, id)
		}
	}
	if len(open) == 0 {
		return 0, false
	}
	return slices.Min(open), true
}

// baseBubble is one claim the scan found.
type baseBubble struct {
	base   fwBase
	status worldStatus
	actID  int32
	own    bool // the Valentine own-base bubble
}

// bubbleVerdict applies the client's bubble rules for s on base b; "" is claimable.
func (sc *fwScan) bubbleVerdict(b fwBase, s worldStatus, recv baseReceiveDict, now time.Time) string {
	_, member := sc.roster.byUID[b.OwnerUID]
	switch {
	case !now.Before(evUnix(s.ExpireTime)):
		return "expired"
	case thanksgivingStatuses[s.ID]:
		switch _, received := recv[receivedPair{b.OwnerUID, s.ID}]; {
		case b.OwnerUID == "" || b.OwnerUID == sc.ownUID:
			return "own base"
		case !member:
			return "not an alliance member's base"
		case b.ServerID != b.SrcServerID || int64(b.ServerID) != sc.server:
			return "not on its owner's home server"
		case received:
			return "already received"
		}
		return ""
	case valentineStatuses[s.ID] != (valentineStatus{}):
		if b.OwnerUID == "" || b.OwnerUID != sc.ownUID {
			return "another player's base (away-from-home claim or scout march: never)"
		}
		if exp, ok := recv[receivedPair{b.OwnerUID, s.ID}]; ok && now.Before(evUnix(exp*1000)) {
			return "already received"
		}
		n := 0
		for p, exp := range recv {
			if p.statusID == s.ID && now.Before(evUnix(exp*1000)) {
				n++
			}
		}
		if n >= valentineStatuses[s.ID].dayCap {
			return "day cap reached"
		}
		return ""
	}
	return "not a reward bubble"
}

func runBaseBubbles(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	tgAct, tgOpen := openActivityOf(in, thanksgivingActivities, thanksgivingActivityType)
	var valentineActs []int32
	for _, v := range valentineStatuses {
		valentineActs = append(valentineActs, v.activityID)
	}
	_, valOpen := openActivityOf(in, valentineActs, valentineActivityType)
	if !tgOpen && !valOpen {
		slog.Info(baseBubblesFeature + ": no Thanksgiving or Valentine activity open; skipping")
		return nil
	}
	sc, err := sharedAllianceTileScan(conn, in, baseBubblesFeature)
	if sc == nil || session.ContainsNonTimeoutNetError(err) {
		return err
	}
	errs := []error{err}
	now := sc.now
	recv := newBaseReceiveDict(in)
	count, countKnown := thanksgivingCount(in.Raw, tgAct, now)
	slog.Info(baseBubblesFeature+": state", "thanksgivingActivity", tgAct, "thanksgivingOpen", tgOpen, "valentineOpen", valOpen,
		"thanksgivingToday", count, "thanksgivingTodayKnown", countKnown, "receivedPairs", len(recv), "hqsWithStatuses", len(sc.bases))

	var picks []baseBubble
	for _, b := range sc.bases {
		for _, s := range b.Statuses {
			if !thanksgivingStatuses[s.ID] && valentineStatuses[s.ID] == (valentineStatus{}) {
				continue
			}
			why := sc.bubbleVerdict(b, s, recv, now)
			bb := baseBubble{base: b, status: s, actID: tgAct, own: valentineStatuses[s.ID] != (valentineStatus{})}
			switch {
			case why != "":
			case bb.own:
				bb.actID = valentineStatuses[s.ID].activityID
				if !valOpenFor(in, bb.actID) {
					why = "its Valentine activity is not open"
				}
			case !tgOpen:
				why = "no Thanksgiving activity open"
			}
			slog.Info(baseBubblesFeature+": bubble", "owner", sc.ref(b.OwnerUID), "statusId", s.ID, "layer", s.Layer,
				"expiresInMin", int64(evUnix(s.ExpireTime).Sub(now).Minutes()), "eligible", why == "", "reason", why)
			if why == "" {
				picks = append(picks, bb)
			}
		}
	}
	slices.SortStableFunc(picks, func(a, b baseBubble) int {
		return cmp.Or(cmp.Compare(a.status.ExpireTime, b.status.ExpireTime), cmp.Compare(a.base.PointID, b.base.PointID),
			cmp.Compare(a.status.ID, b.status.ID))
	})
	got := 0
	for i, bb := range picks {
		if got >= baseBubblesMaxClaims {
			break
		}
		if !bb.own && count >= thanksgivingDayReceiveNum {
			slog.Info(baseBubblesFeature+": the day's Thanksgiving bubbles are used up", "today", count, "cap", thanksgivingDayReceiveNum)
			continue
		}
		if i > 0 {
			fwSleep(fireworksClaimPace)
		}
		p := sfs.NewSFSObject()
		cmd := thanksgivingClaimCmd
		p.PutInt("activityId", bb.actID)
		if bb.own {
			cmd = valentineOwnerClaimCmd
		} else {
			p.PutUtfString("otherUid", bb.base.OwnerUID)
		}
		p.PutInt("statusId", bb.status.ID)
		resp, code, err := fwRequest(conn, baseBubblesFeature+": bubble claim", cmd, p)
		if err != nil {
			errs = append(errs, err)
			break
		}
		who := []any{"cmd", cmd, "owner", sc.ref(bb.base.OwnerUID), "statusId", bb.status.ID, "activityId", bb.actID}
		if code != "" {
			slog.Warn(baseBubblesFeature+": claim refused; stopping this run", append(who, "errorCode", code, "errorMsg", evString(resp, "errorMsg"))...)
			errs = append(errs, fmt.Errorf("%s: %s: errorCode=%s", baseBubblesFeature, cmd, code))
			break
		}
		got++
		recv[receivedPair{bb.base.OwnerUID, bb.status.ID}] = bb.status.ExpireTime / 1000
		if !bb.own {
			if n, ok := thanksgivingCount(resp, bb.actID, now); ok && n > count {
				count = n
			} else {
				count++
			}
		}
		slog.Info(baseBubblesFeature+": claimed a bubble", append(who, "thanksgivingToday", count, "reward", fwReward(resp),
			"replyKeys", resp.Keys())...)
	}
	slog.Info(baseBubblesFeature+": claims done", "claimed", got, "eligible", len(picks))
	return errors.Join(errs...)
}

// valOpenFor reports whether the Valentine activity act is open in init.
func valOpenFor(in *Init, act int32) bool {
	_, ok := openActivityOf(in, []int32{act}, valentineActivityType)
	return ok
}
