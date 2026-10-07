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
// Init carries dailySalaryStatus and salaryActivityState (AllianceMilitaryPayDataManager.lua:29-34).
// SalaryStatus is 0 NotAchieved, 1 CanGet, 2 HasGot; SalaryActivityState 10 is Open
// (EnumType.lua:18157-18191). The weekly boxes come from `alliance.salary.gain.activity.info {}`,
// whose reply has weeklySalaryInfo[] {configId, status} and dailySalaryInfo {configId, status}; after
// that read the client takes the daily status from dailySalaryInfo (:72-85). A box is claimable at
// status 1 (red dot, :89-104). The salary UI opens only from the alliance panel, so this needs an
// alliance. Static-only: not sent live yet.
const (
	allianceSalaryInfoCmd  = "alliance.salary.gain.activity.info"
	allianceSalaryClaimCmd = "alliance.salary.gain.reward"

	allianceSalaryDailyConfig = 101
	allianceSalaryCanGet      = 1
	allianceSalaryOpen        = 10
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
	if state, _ := claimInt(in.Raw, "salaryActivityState"); state != allianceSalaryOpen {
		slog.Info("alliance-salary: salary activity not open", "salaryActivityState", state)
		return nil
	}

	var due []int64
	dailyStatus, _ := claimInt(in.Raw, "dailySalaryStatus")
	info, err := session.SendAndWait(conn, "alliance salary info", allianceSalaryInfoCmd, sfs.NewSFSObject())
	if session.ContainsNonTimeoutNetError(err) {
		return err
	}
	if err == nil {
		if v, ok := info.Params.Get("dailySalaryInfo"); ok {
			if d, ok := v.Val.(*sfs.SFSObject); ok {
				if s, ok := claimInt(d, "status"); ok {
					dailyStatus = s
				}
			}
		}
		for _, w := range claimObjects(info.Params, "weeklySalaryInfo") {
			id, ok := claimInt(w, "configId")
			if s, _ := claimInt(w, "status"); ok && s == allianceSalaryCanGet && id != allianceSalaryDailyConfig {
				due = append(due, id)
			}
		}
	}
	if dailyStatus == allianceSalaryCanGet {
		due = append([]int64{allianceSalaryDailyConfig}, due...)
	}
	if len(due) == 0 {
		slog.Info("alliance-salary: nothing claimable", "dailySalaryStatus", dailyStatus)
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
