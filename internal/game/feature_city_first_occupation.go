package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance city first-occupation rewards (MASTER G5, aud-alliance/F4), opt-in and static-only.
// When the player's alliance takes a city for the first time, every member gets a free reward.
// The client asks for the list with `world.all.city.reward.info {}` whenever the city data of its
// own server arrives (WorldAllianceCityDataManager.lua:158-161, 349-351;
// WorldAllCityRewardInfoMessage.lua:4-16); each `cityArr` entry carries cityId,
// firstAllianceReward, firstReward, rewardComplete, starArr and ranks, copied as is
// (OccupyRewardInfo.lua:21-43). The siege-success window claims a city's reward with
// `world.alliance.city.first.occupied.reward {cityId: Int}` when HaveFreeRewardToGet holds:
// (firstAllianceReward or firstReward) and not rewardComplete, in Lua truthiness
// (OccupyRewardInfo.lua:49-51; UISiegeSuccessView.lua:298-302; the S0 window sends the same
// command, UIAttackCityS0SuccessView.lua:381-389; WorldAllianceCityFirstOccupiedRewardMessage.lua:
// 4-7). The window's list of new captures is built for alliance members only (:1085-1101), so this
// needs an alliance.
//
// Never sent: `world.alliance.city.star.reward {cityId, uuid}` and its like-all form, the window's
// other button: a like of another player's siege star, visible to them (StarItem.lua:30-34,
// 75-82); nor the S0 thumbs (city.competition.s0.gain.thumbs.up.reward).
//
// Unverified live: the client sends the info request only after the world city data arrived;
// whether the server answers a session that never loaded it is unknown. The info reply is logged
// at Info without the ranks and stars (other players' names), and the first errorCode of either
// command stops the run.
const (
	cityFirstOccFeature   = "city-first-occupation"
	cityRewardInfoCmd     = "world.all.city.reward.info"
	cityFirstOccClaimCmd  = "world.alliance.city.first.occupied.reward"
	cityFirstOccMaxClaims = 20
)

func init() {
	registerFeature(Feature{
		Name: cityFirstOccFeature,
		Summary: "OPT-IN: claim the free member reward for each city the alliance took for the first time " +
			"(world.alliance.city.first.occupied.reward); never the star likes (static-only, not validated live)",
		Run: runCityFirstOccupation,
	})
}

// cityFirstOccDue is HaveFreeRewardToGet for one cityArr entry.
func cityFirstOccDue(e *sfs.SFSObject) bool {
	return (zoneMobLuaTruthy(e, "firstAllianceReward") || zoneMobLuaTruthy(e, "firstReward")) && !zoneMobLuaTruthy(e, "rewardComplete")
}

func runCityFirstOccupation(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info(cityFirstOccFeature + ": not in an alliance; skipping")
		return nil
	}
	resp, code, err := fwRequest(conn, cityFirstOccFeature+": city reward list", cityRewardInfoCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	if code != "" {
		slog.Warn(cityFirstOccFeature+": city reward list refused", "errorCode", code, "errorMsg", evString(resp, "errorMsg"))
		return fmt.Errorf("%s: %s: errorCode=%s", cityFirstOccFeature, cityRewardInfoCmd, code)
	}
	cities := evObjects(resp, "cityArr")
	var due []int32
	for _, e := range cities {
		id, ok := evNum(e, "cityId")
		claim := ok && id > 0 && id <= math.MaxInt32 && cityFirstOccDue(e)
		slog.Info(cityFirstOccFeature+": city", "cityId", id, "firstAllianceReward", zoneMobLuaTruthy(e, "firstAllianceReward"),
			"firstReward", zoneMobLuaTruthy(e, "firstReward"), "rewardComplete", zoneMobLuaTruthy(e, "rewardComplete"),
			"stars", len(evObjects(e, "starArr")), "ranks", len(evObjects(e, "ranks")), "claim", claim, "keys", e.Keys())
		if claim {
			due = append(due, int32(id))
		}
	}
	slog.Info(cityFirstOccFeature+": city reward list", "cities", len(cities), "claimable", len(due), "replyKeys", resp.Keys())
	if len(due) > cityFirstOccMaxClaims {
		due = due[:cityFirstOccMaxClaims]
	}
	var errs []error
	for i, id := range due {
		if i > 0 {
			fwSleep(fireworksClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutInt("cityId", id)
		r, code, err := fwRequest(conn, cityFirstOccFeature+": claim", cityFirstOccClaimCmd, p)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if code != "" {
			slog.Warn(cityFirstOccFeature+": claim refused; stopping this run", "cityId", id, "errorCode", code, "errorMsg", evString(r, "errorMsg"))
			errs = append(errs, fmt.Errorf("%s: %s cityId %d: errorCode=%s", cityFirstOccFeature, cityFirstOccClaimCmd, id, code))
			break
		}
		slog.Info(cityFirstOccFeature+": claimed", "cityId", id, "reward", fwReward(r), "replyKeys", r.Keys())
	}
	return errors.Join(errs...)
}
