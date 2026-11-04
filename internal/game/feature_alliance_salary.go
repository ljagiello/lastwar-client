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
	// 101's condition "1;200" (39879; AlliancePayTemplate.lua:43-46). It is a table, not a
	// DataConfig row, so init cannot change it.
	allianceSalaryDailyScore = 200
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
			if s, _ := claimInt(w, "status"); ok && s == allianceSalaryCanGet && id != allianceSalaryDailyConfig {
				due = append(due, id)
			}
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
