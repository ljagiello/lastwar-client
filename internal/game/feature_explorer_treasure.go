package game

import (
	"log/slog"

	"lastwar-client/internal/session"
)

// Explorer Treasure chests (MASTER G57), read-only: log the held Explorer Treasure keys and the
// chests they would open. The keys come from Secret Tasks; the chest is opened from the Secret
// Task page's box button, shown only while the Explorer Treasure activity is open
// (DispatchTask.lua:247-251, 1019-1043).
//
// The open button (UIExplorerTreasureView.lua:182-198, the only send site) needs the held key
// count (ExplorerTreasureManager:GetTreasureHaveItemNum, the item's one bag stack,
// ExplorerTreasureManager.lua:298-305) to reach DataConfig explorer_treasure_key k1, the key item
// being k2 (:291-296, 307-312; 5 and 771001 in table item 40413), and the activity of type
// ExploreTreasure 251 (EnumType.lua:6477) to pass CheckIsSend (IsOpen, :325-335; ActivitySweep.Open).
// It sends `hero.dispatch.explorer.treasure.open {}` with no params, at most once a second
// (ExplorerTreasureOpenMessage.lua:4-6). The reply is final: an errorCode, or the reward,
// quality and dispatch_explorer_treasure_guarantee (:8-19).
//
// That open uses up k1 keys a chest, and a new feature spends no items (IMPLEMENT rule 9), so it
// is not sent here: whether to open chests with the keys is the user's decision. The key is goods
// 771001, type 166, whose bag "use" only walks to the explorer (UILWBagMainCtrl.lua:947-949). The
// one type-251 row, activity 94105, is annotated as an internal test in tables 40413, so the
// activity may never be found open. explorer-treasure-plan logs the held key and key box (771002)
// counts every run, whether the activity is open and the chests the open button would allow,
// and sends nothing (MASTER §7: log the key count for a few runs first). Static-only, not
// validated live.
const (
	explorerTreasurePlanFeature        = "explorer-treasure-plan"
	explorerTreasureCmd                = "hero.dispatch.explorer.treasure.open"
	explorerTreasureActType      int32 = 251      // EnumActivity.ExploreTreasure.Type (EnumType.lua:6477)
	explorerTreasureKeysPerChest       = 5        // DataConfig explorer_treasure_key k1 (table item 40413)
	explorerTreasureKeyItem            = "771001" // DataConfig explorer_treasure_key k2 (table item 40413)
	explorerTreasureKeyBox             = "771002" // DataConfig explorer_treasure_key_box k1 (table item 40413)
)

func init() {
	registerFeature(Feature{
		Name: explorerTreasurePlanFeature,
		Summary: "read-only: log the held Explorer Treasure keys and key boxes and the chests (5 keys each) they would open " +
			"while the Explorer Treasure activity is open; sends nothing (static-only, not validated live)",
		DefaultOn: false,
		Run:       runExplorerTreasurePlan,
	})
}

// explorerTreasureState is what the plan logs: the held keys and key boxes (largest init stack),
// whether an Explorer Treasure activity is open, and the chests the open button would allow, one
// per explorerTreasureKeysPerChest keys and none while the activity is closed.
type explorerTreasureState struct {
	keys, keyBoxes, chests int64
	open                   bool
}

func explorerTreasureStateOf(conn *session.GameConn, in *Init) explorerTreasureState {
	held := duelInitItemCounts(in)
	s := explorerTreasureState{keys: held[explorerTreasureKeyItem], keyBoxes: held[explorerTreasureKeyBox],
		open: len(Activities(conn, in).Open(explorerTreasureActType)) > 0}
	if s.open {
		s.chests = s.keys / explorerTreasureKeysPerChest
	}
	return s
}

func runExplorerTreasurePlan(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(explorerTreasurePlanFeature + ": no init push; skipping")
		return nil
	}
	s := explorerTreasureStateOf(conn, in)
	slog.Info(explorerTreasurePlanFeature+": keys held", "keys", s.keys, "keyBoxes", s.keyBoxes,
		"keysPerChest", explorerTreasureKeysPerChest, "activityOpen", s.open, "chestsOpenable", s.chests)
	if s.chests > 0 {
		slog.Info(explorerTreasurePlanFeature+": the open button would allow these chests; not sent (opening spends the keys)",
			"cmd", explorerTreasureCmd, "chests", s.chests)
	}
	return nil
}
