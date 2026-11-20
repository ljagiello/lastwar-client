package game

import (
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance extras (MASTER.md §4 #23), static-only: the weekly star-ceremony rewards. Nothing here
// is visible to other players; the likes are the separate opt-in feature alliance-likes.
//
// The ceremony runs while init allianceStarEndTime (ms) is in the future, with the server-only
// switch alliance_star on and init allianceStarNotify set or alliance.star.gain.activity.info.new
// reporting hasCeremony (AllianceStarManager.lua:85-88, 332-344, 599-636).
// alliance.star.gain.ceremony.info.new {} then returns participateReceive and scratchClaimed:
//   - alliance.star.ceremony.quest.reward.new {} while participateReceive is false
//     (UIAllianceStarMainLeavePanel.lua:38-48, 181-192); the client shows it at the end of the
//     ceremony scene, which is client-only, so whether the server wants the scene first is unverified;
//   - alliance.star.gain.scratch.reward {} once participateReceive is true while scratchClaimed is
//     present and false (AllianceStarManager.lua:365-375).
//
// Not sent: the legacy red envelopes (get.alliance.red.packet -> get.red.pack). The client still
// lists them at login, but nothing in 1.0.364 or 1.0.370 opens the window that claims them. The
// red packets posted in alliance chat (post 611) are the opt-in feature red-packets
// (feature_red_packets.go). The ceremony's emoji (thumbs) reward box is the separate opt-in
// feature alliance-star-emoji.
const (
	allianceStarActivityCmd = "alliance.star.gain.activity.info.new"
	allianceStarInfoCmd     = "alliance.star.gain.ceremony.info.new"
	allianceStarQuestCmd    = "alliance.star.ceremony.quest.reward.new"
	allianceStarScratchCmd  = "alliance.star.gain.scratch.reward"
)

func init() {
	registerFeature(Feature{
		Name:    "alliance-extras",
		Summary: "claim the weekly alliance star-ceremony quest and scratch rewards; static-only",
		Run:     runAllianceExtras,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, allianceStarQuestCmd, allianceStarScratchCmd)
}

// evSwitchOn is DataConfig:CheckSwitch: init dataConfig[key] is 1 (a number or "1")
// (DataConfig.lua:47-82).
func evSwitchOn(in *Init, key string) bool {
	n, ok := evNum(in.Object("dataConfig"), key)
	return ok && n == 1
}

// evInAlliance reports init user.allianceId set (PlayerInfo.lua:567-568).
func evInAlliance(in *Init) bool {
	return evString(in.Object("user"), "allianceId") != ""
}

// allianceStarCeremony applies the ceremony gates above and returns the
// alliance.star.gain.ceremony.info.new reply, or nil when there is no ceremony or a send failed
// (recorded in b). alliance-extras and alliance-star-emoji both read the ceremony through it.
func allianceStarCeremony(b *evBatch, in *Init) *session.ExtensionMessage {
	if in == nil || in.Raw == nil || !evInAlliance(in) || !evSwitchOn(in, "alliance_star") {
		return nil
	}
	end, ok := evNum(in.Raw, "allianceStarEndTime")
	if !ok || evNow().UnixMilli() >= end {
		return nil
	}
	if !evBool(in.Raw, "allianceStarNotify") {
		msg := b.send("alliance star activity", allianceStarActivityCmd, sfs.NewSFSObject())
		if msg == nil || !evBool(msg.Params, "hasCeremony") {
			return nil
		}
	}
	return b.send("alliance star ceremony", allianceStarInfoCmd, sfs.NewSFSObject())
}

func runAllianceExtras(conn *session.GameConn, in *Init) error {
	b := &evBatch{conn: conn}
	msg := allianceStarCeremony(b, in)
	if msg == nil {
		return b.err()
	}
	participated := evBool(msg.Params, "participateReceive")
	if !participated {
		// a reply (success, or the benign already-claimed code) means the quest reward is taken
		participated = b.send("alliance star quest reward", allianceStarQuestCmd, sfs.NewSFSObject()) != nil
	}
	if msg.Params.Has("scratchClaimed") && !evBool(msg.Params, "scratchClaimed") && participated && !b.dead {
		b.send("alliance star scratch card", allianceStarScratchCmd, sfs.NewSFSObject())
	}
	return b.err()
}
