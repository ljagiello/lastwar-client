package sfs

// Reviewed keys of the season-week-card feature (internal/game/feature_season_week_card.go), kept
// apart from knownNonSensitiveSFSKeys only so parallel work packages don't edit the same map; fold
// them into it when merging.
//
//   - "card_id": the season week card's table id (season_weekcard 10001-10013), a gameplay
//     identifier like "type", not a credential or PII (LWSeasonWeekCardInfoMessage.lua:4-7).
func init() {
	knownNonSensitiveSFSKeys["card_id"] = true
}
