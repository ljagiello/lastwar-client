package game

import (
	"fmt"
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Explorer Treasure chests (MASTER G57), OPT-IN: open the chests the held Explorer Treasure keys
// pay for. The keys come from Secret Tasks; the chest is opened from the Secret Task page's box
// button, shown only while the Explorer Treasure activity is open (DispatchTask.lua:247-251,
// 1019-1043).
//
// The open button (UIExplorerTreasureView.lua:182-198, the only send site) needs the held key
// count (ExplorerTreasureManager:GetTreasureHaveItemNum, the item's one bag stack,
// ExplorerTreasureManager.lua:298-305) to reach DataConfig explorer_treasure_key k1, the key item
// being k2 (:291-296, 307-312; 5 and 771001 in table item 40413), and the activity of type
// ExploreTreasure 251 (EnumType.lua:6477) to pass CheckIsSend (IsOpen, :325-335; ActivitySweep.Open).
// It sends `hero.dispatch.explorer.treasure.open {}` with no params, at most once a second
// (ExplorerTreasureOpenMessage.lua:4-6). The reply is final: an errorCode, or the reward,
// quality and dispatch_explorer_treasure_guarantee (:8-19); the result window's five key taps
// play a local shuffle of quality (UIExplorerTreasureRewardView.lua:112-139). Here one chest is
// opened per k1 keys held, at most explorerTreasureMaxPerRun a run, a second apart, stopping on
// the first errorCode or failure.
//
// The key is goods 771001, type 166, whose bag "use" only walks to the explorer
// (UILWBagMainCtrl.lua:947-949): opening chests is its only use. The one type-251 row, activity
// 94105, is annotated as an internal test in tables 40413, so this may never find the activity
// open; the held key and key box (771002) counts are logged every run to show what the account
// holds. explorer-treasure-plan logs the same line and what it would open, and sends nothing: the
// read-only first step (MASTER §7: log the key count for a few runs before enabling the send).
// Static-only, not validated live.
const (
	explorerTreasureFeature            = "explorer-treasure"
	explorerTreasurePlanFeature        = "explorer-treasure-plan"
	explorerTreasureCmd                = "hero.dispatch.explorer.treasure.open"
	explorerTreasureActType      int32 = 251      // EnumActivity.ExploreTreasure.Type (EnumType.lua:6477)
	explorerTreasureKeysPerChest       = 5        // DataConfig explorer_treasure_key k1 (table item 40413)
	explorerTreasureKeyItem            = "771001" // DataConfig explorer_treasure_key k2 (table item 40413)
	explorerTreasureKeyBox             = "771002" // DataConfig explorer_treasure_key_box k1 (table item 40413)
	explorerTreasureMaxPerRun          = 10
	explorerTreasureGap                = time.Second // the open button's 1000 ms throttle
)

// explorerTreasureSleep is time.Sleep; tests replace it.
var explorerTreasureSleep = time.Sleep

func init() {
	registerFeature(Feature{
		Name: explorerTreasureFeature,
		Summary: "OPT-IN: open Explorer Treasure chests with the held keys (5 keys a chest) while the Explorer Treasure " +
			"activity is open; logs the key count every run (static-only, not validated live)",
		DefaultOn: false,
		Run:       runExplorerTreasure,
	})
	registerFeature(Feature{
		Name: explorerTreasurePlanFeature,
		Summary: "read-only: log the held Explorer Treasure keys and key boxes and the chests explorer-treasure would open; " +
			"sends nothing (static-only, not validated live)",
		DefaultOn: false,
		Run:       runExplorerTreasurePlan,
	})
}

func runExplorerTreasure(conn *session.GameConn, in *Init) error {
	return explorerTreasureRun(conn, in, false)
}

func runExplorerTreasurePlan(conn *session.GameConn, in *Init) error {
	return explorerTreasureRun(conn, in, true)
}

func explorerTreasureRun(conn *session.GameConn, in *Init, dry bool) error {
	name := explorerTreasureFeature
	if dry {
		name = explorerTreasurePlanFeature
	}
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	held := duelInitItemCounts(in)
	keys := held[explorerTreasureKeyItem]
	open := len(Activities(conn, in).Open(explorerTreasureActType)) > 0
	chests := min(keys/explorerTreasureKeysPerChest, explorerTreasureMaxPerRun)
	slog.Info(name+": keys held", "keys", keys, "keyBoxes", held[explorerTreasureKeyBox],
		"keysPerChest", explorerTreasureKeysPerChest, "activityOpen", open, "chestsThisRun", chests)
	if !open || chests <= 0 {
		return nil
	}
	if dry {
		slog.Info(name+": WOULD open chests", "cmd", explorerTreasureCmd, "chests", chests)
		return nil
	}
	for i := range chests {
		if i > 0 {
			explorerTreasureSleep(explorerTreasureGap)
		}
		label := fmt.Sprintf("%s: chest %d of %d", explorerTreasureFeature, i+1, chests)
		msg, err := session.SendAndWait(conn, label, explorerTreasureCmd, sfs.NewSFSObject())
		if err != nil {
			return err
		}
		quality, _ := claimInt(msg.Params, "quality")
		guarantee, _ := claimInt(msg.Params, "dispatch_explorer_treasure_guarantee")
		slog.Info(label+" opened", "quality", quality, "guarantee", guarantee, "granted", describeGrants(msg.Params))
	}
	return nil
}
