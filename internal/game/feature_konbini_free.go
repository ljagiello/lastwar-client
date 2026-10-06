package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Konbini free buys (MASTER.md §4 #24), static-only and OPT-IN. buy.in.konbini {index: Int,
// useFree: Bool} buys one convenience-store slot; useFree true is the free buy, false spends item
// 200031 or diamonds (UIKonbiniView.lua:127-156, 298-310). This sends only useFree true, and never
// false.
//
// The client offers the free buy while today's free buys (init konbiniInfo.freeBuyCount, or 0 when
// its lastBuyTime is not today) are below the building level's para2 plus effect 30025
// (KONBINI_EXTRA_FREE_COUNT) (PlayerInfo.lua:172-174, 1135-1144). The konbini (729000) has no rows in
// the 1.0.364 building tables, so para2 is unknown here: this counts only effect 30025, a lower bound
// on the free buys, so it never sends useFree when no free buy is left. In 1.0.364 the building
// does not exist and this never fires.
const (
	konbiniBuyCmd        = "buy.in.konbini"
	konbiniBuildingID    = 729000 // FUN_BUILD_KONBINI (EnumType.lua:3550)
	konbiniExtraFreeFX   = 30025  // KONBINI_EXTRA_FREE_COUNT
	konbiniFirstSlotIdx  = 1      // 1-based slot; every slot shares one daily free counter
	konbiniUseFreeAlways = true
)

func init() {
	registerFeature(Feature{
		Name:    "konbini-free",
		Summary: "OPT-IN: take the konbini's free buy (useFree:true only, never paid); dormant in 1.0.364; static-only",
		Run:     runKonbiniFree,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, konbiniBuyCmd)
}

func runKonbiniFree(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !evHasBuilding(in, konbiniBuildingID) {
		return nil
	}
	info := in.Object("konbiniInfo")
	if info == nil {
		return nil
	}
	used, _ := evNum(info, "freeBuyCount")
	last, _ := evNum(info, "lastBuyTime")
	today, ok := evClaimedToday(in, last)
	if !ok {
		return nil
	}
	if !today {
		used = 0
	}
	if free := evInitEffect(in, konbiniExtraFreeFX); free <= used {
		slog.Debug("konbini: no free buy provably left", "freeKnown", free, "used", used)
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutInt("index", konbiniFirstSlotIdx)
	p.PutBool("useFree", konbiniUseFreeAlways)
	_, err := session.SendAndWait(conn, "konbini free buy", konbiniBuyCmd, p)
	return err
}
