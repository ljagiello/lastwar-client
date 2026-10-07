package game

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Free hero recruit and free survivor (worker) draw (MASTER.md §4 #6). OPT-IN: the same commands
// with useFree 0 are paid pulls (§6), so every request here is built with useFree 1 and isTen 0
// only, and only behind the client's own free-pull gate.
//
// Hero: init lotteryFreeInfo[] {id, type?, dailyFreeLimit, dailyFreeNextFreshTime (seconds),
// startTime/endTime (seconds), item "itemId;num|..."} (LotteryDataManager.lua:102-121;
// LotteryInfo.lua:117-200). The UI offers the free pull when dailyFreeLimit > 0 and
// now_s >= dailyFreeNextFreshTime (IsSupportFreeRecruit / CanFreeRecruit, LotteryInfo.lua:345-352;
// UIHeroRecruitView.lua:931-937) on an open pool (IsOpen, :323-329); pools of type 2 or 3 are not
// listed (LotteryDataManager.lua:118-120). Here dailyFreeNextFreshTime must also be present and
// positive, so a missing timer never reads as "free now". The pull is
// `lottery.hero.card {id: UtfString, isTen: 0, useFree: 1, aiPushStatus: SFSObject, itemId}` with
// itemId the pool's first cost item, as the UI sends it (LotteryHeroCardMessage.lua:4-17;
// UIHeroRecruitView.lua:931-943), and aiPushStatus the client's default AI-chat push switches
// (LWChatAIManager.lua:56-67, 157-165; ai_switch rows 106, 107, 110, 111, 112, all on by default).
// Whether the server needs either is open (MASTER.md §8.2 #10).
//
// Worker: `worker.lottery.info {officerId: Int}` returns nextFreeTime (ms), and
// `lottery.worker.card {useFree: 1, isTen: 0, officerId: Int}` draws when now >= nextFreeTime
// (WorkerLotteryInfoMessage.lua:4-7; LotteryWorkerCardMessage.lua:4-9; WorkerLotteryInfo.lua:26-30).
// officerId is the first of init workerOfficerIds (LotteryDataManager.lua:122-137, 215-218).
// Static-only: not sent live yet.
const (
	freeRecruitsHeroCmd       = "lottery.hero.card"
	freeRecruitsWorkerInfoCmd = "worker.lottery.info"
	freeRecruitsWorkerCmd     = "lottery.worker.card"
)

// freeRecruitsAIPushSwitches are the ai_switch ids the client puts in aiPushStatus.
var freeRecruitsAIPushSwitches = []string{"106", "107", "110", "111", "112"}

func init() {
	registerFeature(Feature{
		Name:    "free-recruits",
		Summary: "OPT-IN: take the free hero recruit and free survivor draw when the free timer allows (useFree 1 only)",
		// The pulls score in the duel (hero Thu, survivor Tue) but a free pull does not stack, so a
		// held one would be lost: always run (DUEL.md §5).
		Duel: []DuelScore{{Type: DuelScoreRecruitHero}, {Type: DuelScoreRecruitSurvivor}},
		Run:  runFreeRecruits,
	})
	session.RegisterBenignErrorCode("120289", freeRecruitsHeroCmd, freeRecruitsWorkerCmd)
}

// freeRecruitsParams builds a pull request. It is the only place these commands' params are made,
// and it always sends useFree 1 and isTen 0.
func freeRecruitsParams() *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutInt("isTen", 0)
	p.PutInt("useFree", 1)
	return p
}

// freeRecruitsHeroDue returns the hero pools whose free pull is available now.
func freeRecruitsHeroDue(in *Init) []*sfs.SFSObject {
	now := claimNow(in)
	var out []*sfs.SFSObject
	for _, pool := range in.Objects("lotteryFreeInfo") {
		id, ok := claimString(pool, "id")
		if !ok || id == "" {
			continue
		}
		if t, ok := claimInt(pool, "type"); ok && (t == 2 || t == 3) {
			continue
		}
		limit, _ := claimInt(pool, "dailyFreeLimit")
		next, ok := claimInt(pool, "dailyFreeNextFreshTime")
		if limit <= 0 || !ok || next <= 0 || now.Unix() < next {
			continue
		}
		start, _ := claimInt(pool, "startTime")
		end, _ := claimInt(pool, "endTime")
		if end > start && (now.Unix() <= start || now.Unix() >= end) {
			continue
		}
		out = append(out, pool)
	}
	return out
}

func runFreeRecruits(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("free-recruits: no init push; skipping")
		return nil
	}
	var errs []error
	for _, pool := range freeRecruitsHeroDue(in) {
		id, _ := claimString(pool, "id")
		p := freeRecruitsParams()
		p.PutUtfString("id", id)
		push := sfs.NewSFSObject()
		for _, k := range freeRecruitsAIPushSwitches {
			push.PutBool(k, true)
		}
		p.PutSFSObject("aiPushStatus", push)
		if item, ok := claimString(pool, "item"); ok {
			if first, _, _ := strings.Cut(item, ";"); first != "" {
				p.PutUtfString("itemId", first)
			}
		}
		_, err := claimAndLog(conn, "free hero recruit "+id, freeRecruitsHeroCmd, p)
		if err != nil {
			errs = append(errs, err)
		}
		if session.ContainsNonTimeoutNetError(err) {
			return errors.Join(errs...)
		}
	}
	if err := freeRecruitsWorker(conn, in); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func freeRecruitsWorker(conn *session.GameConn, in *Init) error {
	ids := claimInts(in.Raw, "workerOfficerIds")
	if len(ids) == 0 {
		slog.Info("free-recruits: init has no workerOfficerIds; skipping the survivor draw")
		return nil
	}
	officer := int32(ids[0])
	q := sfs.NewSFSObject()
	q.PutInt("officerId", officer)
	info, err := session.SendAndWait(conn, "survivor recruit info", freeRecruitsWorkerInfoCmd, q)
	if err != nil {
		return err
	}
	next, ok := claimInt(info.Params, "nextFreeTime")
	if !ok || next <= 0 || claimNow(in).UnixMilli() < next {
		slog.Info("free-recruits: free survivor draw not available", "nextFreeTime", next)
		return nil
	}
	p := freeRecruitsParams()
	p.PutInt("officerId", officer)
	_, err = claimAndLog(conn, fmt.Sprintf("free survivor draw %d", officer), freeRecruitsWorkerCmd, p)
	return err
}
