package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Hero-countdown building reward (MASTER G36, cov-dc-social-city/F8), OPT-IN. The newcomer building
// LW_BUILD_HERO_COUNTDOWN (10222000, Global/EnumType.lua:3728) holds a hero until its countdown
// ends: init building_new[].fixCityHeroET (ms, BuildingDate.lua:205-209). Once the server time
// reaches it the client shows the BuildHeroCountdownReady bubble (BuildBubbleManager.lua:3062-3072)
// and its tap sends `free.building.fix.city.hero {uuid: Long}` (BuildHeroCountdownManager.lua:90-98,
// FreeBuildingFixCityHeroMessage.lua:4-7); the hero pop-up sends the same request under the same
// time check (UILWSalaHeroPopView.lua:135-139, 144-148). The reply carries `reward` (:9-14). The
// pop-up's other branch, before the countdown ends, opens the season week-card shop and is never
// taken here. A one-off claim: an account past it no longer has the building. Static-only, not
// validated live.
const (
	heroCountdownFeature  = "hero-countdown-reward"
	heroCountdownCmd      = "free.building.fix.city.hero"
	heroCountdownBuilding = 10222000
)

func init() {
	registerFeature(Feature{
		Name:      heroCountdownFeature,
		Summary:   "OPT-IN: claim the newcomer hero-countdown building's hero once its countdown has ended (free.building.fix.city.hero) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runHeroCountdown,
	})
}

// heroCountdownDue returns the uuids of hero-countdown buildings whose fixCityHeroET has passed.
// The client only needs fixCityHeroET set; a 0 is left alone here, since a cleared countdown is the
// likelier meaning than one that ended at the epoch.
func heroCountdownDue(in *Init, nowMs int64) []int64 {
	var out []int64
	for _, b := range in.Objects("building_new") {
		if id, _ := claimInt(b, "bId"); id != heroCountdownBuilding {
			continue
		}
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		et, ok := claimInt(b, "fixCityHeroET")
		if !ok || et <= 0 {
			continue
		}
		if nowMs < et {
			slog.Info(heroCountdownFeature+": countdown still running", "uuid", uuid, "fixCityHeroET", et)
			continue
		}
		out = append(out, uuid)
	}
	return out
}

func runHeroCountdown(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(heroCountdownFeature + ": no init push; skipping")
		return nil
	}
	due := heroCountdownDue(in, claimNow(in).UnixMilli())
	if len(due) == 0 {
		slog.Info(heroCountdownFeature + ": no ended hero countdown")
		return nil
	}
	// No benign error code is registered (a never-sent command): an errorCode is logged by
	// SendAndWait and stops the claim.
	for _, uuid := range due {
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s %d", heroCountdownFeature, uuid), heroCountdownCmd, p); err != nil {
			return err
		}
	}
	return nil
}
