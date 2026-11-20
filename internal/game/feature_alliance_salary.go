package game

import (
	"errors"
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance salary (military pay, MASTER.md §4 #7): a daily salary (configId 101) and weekly
// salary boxes (configIds 201-205, and 301 for R4/R5), each claimed with
// `alliance.salary.gain.reward {configId: Int}` (AllianceSalaryGainRewardMessage.lua:3-6).
//
// Init carries dailySalaryStatus, dailySalary (the day's score), salaryActivityState and salaryTime
// (AllianceMilitaryPayDataManager.lua:34-39). SalaryStatus is 0 NotAchieved, 1 CanGet, 2 HasGot;
// SalaryActivityState is 0 NotOpen, 10 Open (EnumType.lua:18176-18180, 18210). The activity counts
// as open at state 10, or at state 0 once salaryTime has passed: the alliance page flips the state
// to Open itself when the countdown to salaryTime ends (UIAllianceDailySalaryNode.lua:68-75,
// GetActivityStartTime :70-72); a salaryTime of 0 or none is not taken as passed. The weekly boxes
// come from `alliance.salary.gain.activity.info {}`, whose reply has weeklySalaryInfo[] {configId,
// status}, dailySalaryInfo {configId, status} and dailyUserSalary; after that read the client takes
// the daily status from dailySalaryInfo and the score from dailyUserSalary (:84-97). A weekly box is
// claimable at status 1 (red dot, :103-119). The daily salary is claimable when CanGetDailySalary
// holds (:138-145): a known status that is 1, or a score of at least alliance_pay[101]'s 200 while
// the status is not 2 (the button claims config 101 on that test, LWAllianceMilitaryPayMainView.lua:
// 210-225). The salary UI opens only from the alliance panel, so this needs an alliance.
//
// Two client gates added with the 1.0.370 audit (MASTER B37), each applied only on positive
// evidence so a production account never loses a claim to an init Go cannot read:
//   - the alliance panel shows the salary entry only while init lists an activity of type 372
//     (EnumActivity.AllianceSalary, EnumType.lua:6507) that CheckIsSend accepts
//     (IsFunctionOpen, AllianceMilitaryPayDataManager.lua:171-177; UILWAlMainCtrl.lua:357-360).
//     Nothing is sent when init lists such an entry that is not open, or when the init activity
//     list is not empty, every entry's id resolves in table activity and none is of type 372; an
//     id the generated table lacks (a newer table), an empty list or a malformed entry leaves the
//     gate undecided, and the run goes on as before;
//   - box 301 is listed only for R4/R5 (LWAllianceMilitaryPayMainView.lua:184-188; alliance_pay
//     row 301 show_type 1 = R4orR5, EnumType.lua:18250; tables 40413). It is skipped when init
//     alliance shows a rank below 4 (allianceRank); an unknown rank claims it on status 1 as before.
//
// Static-only: not sent live yet.
const (
	allianceSalaryInfoCmd  = "alliance.salary.gain.activity.info"
	allianceSalaryClaimCmd = "alliance.salary.gain.reward"

	allianceSalaryDailyConfig = 101
	allianceSalaryCanGet      = 1
	allianceSalaryHasGot      = 2
	allianceSalaryNotOpen     = 0
	allianceSalaryOpen        = 10
	// allianceSalaryDailyScore is maxDailySalaryScore, the first value of table alliance_pay row
	// 101's condition "1;200" (39879 and 40413; AlliancePayTemplate.lua:43-46). It is a table, not
	// a DataConfig row, so init cannot change it.
	allianceSalaryDailyScore = 200
	// allianceSalaryActivityType is EnumActivity.AllianceSalary (EnumType.lua:6507); table activity
	// 40413 has one such row, 28.
	allianceSalaryActivityType = 372
	// allianceSalaryOfficerBox is alliance_pay row 301, the only row with show_type 1 (R4orR5).
	allianceSalaryOfficerBox = 301
	// allianceRankR4 is the lowest rank IsR4orR5 accepts (AllianceBaseDataManager.lua:382-388).
	allianceRankR4 = 4
)

func init() {
	registerFeature(Feature{
		Name:    "alliance-salary",
		Summary: "claim the alliance daily salary and any claimable weekly salary boxes",
		Run:     runAllianceSalary,
	})
	session.RegisterBenignErrorCode("120289", allianceSalaryClaimCmd)
}

func runAllianceSalary(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("alliance-salary: no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info("alliance-salary: not in an alliance; skipping")
		return nil
	}
	switch open, known := allianceSalaryListed(in); {
	case known && !open:
		slog.Info("alliance-salary: init lists no open alliance salary activity (type 372); skipping")
		return nil
	case !known:
		slog.Info("alliance-salary: cannot tell from init whether the salary activity (type 372) is listed; going on")
	}
	rank, rankKnown := allianceRank(in)
	state, _ := claimInt(in.Raw, "salaryActivityState")
	salaryTime, _ := claimInt(in.Raw, "salaryTime")
	if !allianceSalaryActivityOpen(state, salaryTime, claimNow(in).UnixMilli()) {
		slog.Info("alliance-salary: salary activity not open", "salaryActivityState", state, "salaryTime", salaryTime)
		return nil
	}

	var due []int64
	dailyStatus, statusKnown := claimInt(in.Raw, "dailySalaryStatus")
	score, scoreKnown := claimInt(in.Raw, "dailySalary")
	info, err := session.SendAndWait(conn, "alliance salary info", allianceSalaryInfoCmd, sfs.NewSFSObject())
	if session.ContainsNonTimeoutNetError(err) {
		return err
	}
	if err == nil {
		if v, ok := info.Params.Get("dailySalaryInfo"); ok {
			if d, ok := v.Val.(*sfs.SFSObject); ok {
				if s, ok := claimInt(d, "status"); ok {
					dailyStatus, statusKnown = s, true
				}
			}
		}
		if n, ok := claimInt(info.Params, "dailyUserSalary"); ok {
			score, scoreKnown = n, true
		}
		for _, w := range claimObjects(info.Params, "weeklySalaryInfo") {
			id, ok := claimInt(w, "configId")
			s, _ := claimInt(w, "status")
			if !ok || s != allianceSalaryCanGet || id == allianceSalaryDailyConfig {
				continue
			}
			if id == allianceSalaryOfficerBox && rankKnown && rank < allianceRankR4 {
				slog.Info("alliance-salary: the R4/R5 box is not listed below R4; skipping it", "configId", id, "rank", rank)
				continue
			}
			due = append(due, id)
		}
	}
	if allianceCanGetDailySalary(dailyStatus, statusKnown, score, scoreKnown) {
		due = append([]int64{allianceSalaryDailyConfig}, due...)
	}
	if len(due) == 0 {
		slog.Info("alliance-salary: nothing claimable", "dailySalaryStatus", dailyStatus, "dailySalary", score)
		return err
	}
	errs := claimEach(due, func(id int64) error {
		params := sfs.NewSFSObject()
		params.PutInt("configId", int32(id))
		_, err := claimAndLog(conn, fmt.Sprintf("alliance salary %d", id), allianceSalaryClaimCmd, params)
		return err
	})
	return errors.Join(append([]error{err}, errs...)...)
}

// allianceSalaryActivityOpen is the salary activity's open test (see the file comment).
func allianceSalaryActivityOpen(state, salaryTimeMs, nowMs int64) bool {
	return state == allianceSalaryOpen || state == allianceSalaryNotOpen && salaryTimeMs > 0 && salaryTimeMs <= nowMs
}

// allianceCanGetDailySalary is CanGetDailySalary (AllianceMilitaryPayDataManager.lua:138-145): false
// without a status; else status 1, or a known score of at least allianceSalaryDailyScore while the
// status is not 2.
func allianceCanGetDailySalary(status int64, statusKnown bool, score int64, scoreKnown bool) bool {
	if !statusKnown {
		return false
	}
	if status == allianceSalaryCanGet {
		return true
	}
	return scoreKnown && score >= allianceSalaryDailyScore && status != allianceSalaryHasGot
}

// allianceSalaryListed is IsFunctionOpen (AllianceMilitaryPayDataManager.lua:171-177) where init
// settles it: open when init lists a type-372 activity that CheckIsSend accepts (Activity.open);
// known and closed when it lists one that is not open, or when every init activity id resolves in
// table activity and none is type 372. known is false without an init activity field, or when
// some id is missing from the generated table (it could be a newer salary row), or the list is
// empty or holds an entry that is not an object: none of those is evidence of a closed salary.
func allianceSalaryListed(in *Init) (open, known bool) {
	entries, skipped, ok := initActivityEntries(in)
	if !ok || len(entries) == 0 || skipped > 0 {
		return false, false
	}
	now, hq := claimNow(in).UnixMilli(), activityHQLevel(in)
	allResolved, listed := true, false
	for _, e := range entries {
		id, ok := activityEntryID(e.obj, e.key)
		if !ok {
			allResolved = false
			continue
		}
		typ, known := activityTypeOf(id)
		if !known {
			allResolved = false
			continue
		}
		if typ != allianceSalaryActivityType {
			continue
		}
		listed = true
		if (Activity{ID: id, Type: typ, NeedHQ: activityNeedHQ(id), Raw: e.obj}).open(now, hq) {
			return true, true
		}
	}
	return false, listed || allResolved
}

// initActivityEntry is one entry of init `activity` with the key it was listed under.
type initActivityEntry struct {
	key string
	obj *sfs.SFSObject
}

// initActivityEntries returns init `activity`'s entries the way parseActivities walks them (an
// object keyed by id or an array, ActivityListDataManager's table.walk), including ids the
// generated table lacks, and how many entries were skipped as not objects. ok is false when init
// has no activity field or it is neither shape.
func initActivityEntries(in *Init) (entries []initActivityEntry, skipped int, ok bool) {
	if in == nil || in.Raw == nil {
		return nil, 0, false
	}
	v, ok := in.Raw.Get("activity")
	if !ok {
		return nil, 0, false
	}
	switch c := v.Val.(type) {
	case *sfs.SFSObject:
		for _, k := range c.Keys() {
			if o := objectField(c, k); o != nil {
				entries = append(entries, initActivityEntry{k, o})
			} else {
				skipped++
			}
		}
	case *sfs.SFSArray:
		for _, it := range c.Items() {
			if o, ok := it.Val.(*sfs.SFSObject); ok {
				entries = append(entries, initActivityEntry{"", o})
			} else {
				skipped++
			}
		}
	default:
		return nil, 0, false
	}
	return entries, skipped, true
}

// allianceRank is the player's rank in the alliance as AllianceBaseInfo:ParseData reads it from
// init alliance (AllianceBaseInfo.lua:202-232): rank when present and above 0 (ranks run 1-5; a 0
// is the Lua default, not a rank), else 5 when learderUid (the wire's spelling) is the player. ok
// is false when init says neither.
func allianceRank(in *Init) (int64, bool) {
	al := in.Object("alliance")
	if r, ok := claimInt(al, "rank"); ok && r > 0 {
		return r, true
	}
	if leader := idString(al, "learderUid"); leader != "" && leader == ownUID(in) {
		return 5, true
	}
	return 0, false
}
