package game

import (
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Free daily stamina (MASTER.md §4 #5): every account may claim role_stamina.k4 (50) stamina
// role_stamina.k2 (2) times per server day, at least role_stamina.k3 (14,400 s) apart.
//
// The 1.0.364 client keeps todayFreeStamina and lastClaimFreeStaminaTime (ms) from init
// (InitMessage.lua:321), from `user.get.daily.stamina.info` (which it re-sends 5-15 s after each
// day reset; a missing todayFreeStamina means 0, UserGetDailyStaminaInfoMessage.lua:12-15) and from
// the claim reply (PlayerInfo.lua:250-266). Its countdown (Util/UIUtil.lua:3880-3898) treats the
// claim as available when both fields are 0, or when todayFreeStamina < k2 and the k3 cooldown
// since the last claim has run out; the claim itself is the parameterless
// `user.claim.daily.stamina` (LWResourceLackCell.lua:1986-1991). The real client only claims from
// the resource-lack popup. Static-only: not sent live yet.
const (
	dailyStaminaInfoCmd  = "user.get.daily.stamina.info"
	dailyStaminaClaimCmd = "user.claim.daily.stamina"

	// role_stamina k2 and k3 from table item (1.0.364; identical in live 39516). DataConfig keeps
	// init.dataConfig but then overwrites every row the local item table has
	// (DataConfig.lua:47-66, 34-44), so the table values are the ones the client uses.
	dailyStaminaClaimsPerDay = 2
	dailyStaminaCooldown     = 14400 * time.Second
)

func init() {
	registerFeature(Feature{
		Name:    "daily-stamina",
		Summary: "claim the free daily stamina (2 x 50 per server day, 4 h apart)",
		Run:     runDailyStamina,
	})
	// 120289 is the server's generic "already executed this command" (live for the VIP claims).
	session.RegisterBenignErrorCode("120289", dailyStaminaClaimCmd)
}

// dailyStaminaEligible reports whether a claim is due: fewer than the daily number of claims so
// far and the cooldown since lastClaimMs has passed (lastClaimMs 0 means never claimed).
func dailyStaminaEligible(todayClaims, lastClaimMs int64, now time.Time) bool {
	if todayClaims >= dailyStaminaClaimsPerDay {
		return false
	}
	return now.UnixMilli()-lastClaimMs >= dailyStaminaCooldown.Milliseconds()
}

func runDailyStamina(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("daily-stamina: no init push; skipping")
		return nil
	}
	lastClaim, _ := claimInt(in.Raw, "lastClaimFreeStaminaTime")

	msg, err := session.SendAndWait(conn, "daily stamina info", dailyStaminaInfoCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	todayClaims, _ := claimInt(msg.Params, "todayFreeStamina")
	if v, ok := claimInt(msg.Params, "lastClaimFreeStaminaTime"); ok {
		lastClaim = v
	}
	if !dailyStaminaEligible(todayClaims, lastClaim, claimNow(in)) {
		slog.Info("daily-stamina: nothing to claim", "todayFreeStamina", todayClaims, "lastClaimFreeStaminaTime", lastClaim)
		return nil
	}
	resp, err := claimAndLog(conn, "daily stamina claim", dailyStaminaClaimCmd, sfs.NewSFSObject())
	if err == nil && resp != nil && !resp.Params.Has("errorCode") {
		n, _ := claimInt(resp.Params, "todayFreeStamina")
		slog.Info("daily-stamina: claimed", "todayFreeStamina", n)
	}
	return err
}
