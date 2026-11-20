package sfs

// Request keys put on the wire by the 1.0.370 audit's WP9 opt-in event claims (internal/game:
// feature_event_leftovers.go, feature_tactical_cards.go, feature_offseason_tasks.go). Each is a game
// id, a count or a flag of a claim request; none authenticates anything or identifies a person.
func init() {
	for _, k := range []string{
		"all",       // Counter Attack claim-all flag (DarkKnightAllianceAwardRoundMessage.lua:4-14)
		"cardType",  // tactical card type 1-3 (TacticalCard/ClaimBattleCardStageRewardMessage.lua:5-9)
		"cfgId",     // battle_card_reward row id (TacticalCard/ClaimBattleCardStageRewardMessage.lua:5-9)
		"chapter",   // sky-battle chapter id (LwGetSkyBattleStageMessage.lua:4-7)
		"group",     // off-season task group 1/2 (CityBattleS1RestGainTaskInfoMessage.lua:4-7)
		"round",     // Counter Attack round (DarkKnightAllianceAwardRoundMessage.lua:4-14)
		"stageType", // sky-battle mode 0/1 (LwGetSkyBattleStageMessage.lua:4-7)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
