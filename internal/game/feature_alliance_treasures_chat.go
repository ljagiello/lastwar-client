package game

import (
	"cmp"
	"errors"
	"log/slog"
	"slices"

	"lastwar-client/internal/chat"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// alliance-treasures-chat (OPT-IN, static-only; MASTER G56): claim the radar treasures the
// alliance chat's "{owner}'s treasure at (x, y) has been dug up!" cards (post 600,
// Detect_Treasure_Fin_Info) point at. alliance-treasures finds treasures only within
// atBlockRadius blocks of a member HQ, a guessed radius; the card names the treasure wherever it is.
//
// The cards come from this run's one chat read (chatCards, shared with chat-scan, red-packets and
// fireworks-chat). A tap on the card (ChatPushMsg:OnPointerClick, ChatPushMsg.lua:51-70) sends
//
//	detect.event.get.treasure.claim.info {uuid: Long, source: Int 1, targetServer: Int}
//
// with uuid and targetServer from the card's customJsonParam, source SeeDetectEventGetTreasureClaimInfoType.Chat
// (1, EnumType.lua:15270) and, when the card has no targetServer, PlayerInfo:GetSourceServerId
// (DetectEventGetTreasureClaimInfoMessage.lua:5-17). The reply {cfgId, num, pointId, ownerInfo,
// array[{uid, ...}], reward, createTime, targetServer} (DetectEventGetTreasureClaimInfo.lua:30-81)
// moves the camera to pointId on targetServer, else the source server, and with pointId 0 opens
// the claim-info window instead (UIChatView_v2.lua:643-659; RadarCenterDataManager.lua:872-882).
// On the map the claim is a tap on the treasure point, alliance-treasures' own claim. So this
// sends the info request per card, reads the blocks holding the points (allianceTilePoints), and
// applies alliance-treasures' rules to the treasure with the card's uuid (fwScan.verdict) before
// the same detect.event.claim.treasure (fwScan.claimTreasures).
//
// Gates, in order:
//   - the card's cross-server check (ChatPushMsg.lua:52-61): only on the login source server
//     (PlayerInfo:IsLoginSourceServer). Its exceptions for post 600, UIUtil.CheckDetectCanCrossServer
//     and the nine-nation truce, are not modelled, so away from it nothing is sent;
//   - the card has a uuid; this run has not answered the treasure yet (alliance-treasures, run
//     just before by name, shares tileClaims) nor stopped this run's treasure claims;
//   - the reply has no errorCode (it skips that card; no code is registered as benign), num > 0
//     (DetectEventGetTreasureClaimInfo reads it as remainNum, 0 when absent; the client's chat
//     path does not test it, and nothing shows it, so its meaning is unverified), the player's
//     uid is not in array (GetSelfClaimState, :83-96), pointId > 0, on the player's own server
//     and the 1000-tile map;
//   - on the tile: every alliance-treasures rule (dug, not expired, this alliance's, not the
//     player's own, not full, radar types 1/3/11 of treasure events).
//
// Lookups take the newest cards first, atClaimPace apart, at most atMaxClaimsPerRun less the
// treasures this run already answered. A claim is as visible as alliance-treasures' (the
// treasure's claimer list, a server post on a double reward); the read itself never posts. Like
// alliance-treasures, no Duel score: claiming an ally's treasure completes no radar task.

const (
	allianceTreasuresChatFeature = "alliance-treasures-chat"
	allianceTreasureInfoCmd      = "detect.event.get.treasure.claim.info"
	atInfoSourceChat             = 1 // SeeDetectEventGetTreasureClaimInfoType.Chat (EnumType.lua:15270)
)

func init() {
	registerFeature(Feature{
		Name: allianceTreasuresChatFeature,
		Summary: "OPT-IN: claim the radar treasures the alliance chat's \"treasure ... has been dug up!\" cards point at " +
			"(detect.event.get.treasure.claim.info, the treasure's world block, detect.event.claim.treasure, free, no march); " +
			"reads chat, never posts; your name shows in the treasure's claimer list (static-only, not validated live)",
		Run: runAllianceTreasuresChat,
	})
}

// atChatCard is one treasure-dug card to look up.
type atChatCard struct {
	post chat.Post
	t    chat.TreasureDug
}

// treasureCards logs every treasure-dug card with its verdict and returns those to look up,
// newest first, each treasure once.
func treasureCards(res *chat.Result, me chatSelf, nowMs int64, answered map[int64]bool) []atChatCard {
	var out []atChatCard
	seen := map[int64]bool{}
	for _, p := range res.Posts {
		if p.Type != chat.PostTreasureDug {
			continue
		}
		t, err := p.TreasureDug()
		why := ""
		switch {
		case err != nil:
			why = "unreadable card: " + err.Error()
		case answered[t.UUID]:
			why = "already answered this run"
		case seen[t.UUID]:
			why = "duplicate card"
		}
		ageMin := int64(-1)
		if p.ServerTimeMs > 0 {
			ageMin = (nowMs - p.ServerTimeMs) / 60000
		}
		slog.Info(allianceTreasuresChatFeature+": treasure-dug card", "seqId", p.SeqID, "uuid", t.UUID,
			"targetServer", t.TargetServer, "hasTargetServer", t.HasTarget, "eventId", t.EventID,
			"sender", senderRef(p, me), "ageMin", ageMin, "lookUp", why == "", "reason", why)
		if why == "" {
			seen[t.UUID] = true
			out = append(out, atChatCard{post: p, t: t})
		}
	}
	slices.SortStableFunc(out, func(a, b atChatCard) int {
		return cmp.Or(cmp.Compare(b.post.ServerTimeMs, a.post.ServerTimeMs), cmp.Compare(b.post.SeqID, a.post.SeqID))
	})
	return out
}

// atInfoVerdict reads a detect.event.get.treasure.claim.info reply; "" with the point and its
// server is read the tile.
func atInfoVerdict(resp *sfs.SFSObject, code string, me chatSelf) (point, server int64, why string) {
	point, _ = evNum(resp, "pointId")
	server, _ = evNum(resp, "targetServer")
	if server <= 0 {
		server = me.sourceServer // UIChatView_v2.lua:648-651
	}
	num, _ := evNum(resp, "num")
	claimed := slices.ContainsFunc(evObjects(resp, "array"), func(e *sfs.SFSObject) bool { return evString(e, "uid") == me.uid })
	switch {
	case code != "":
		return point, server, "info refused: " + code
	case num <= 0:
		return point, server, "no claims left (num)"
	case claimed:
		return point, server, "already claimed by you"
	case point <= 0:
		return point, server, "no point (the chat opens the claim window instead)"
	case server != me.selfServer:
		return point, server, "on another server"
	}
	if _, _, ok := fwTilePos(point); !ok {
		return point, server, "outside the 1000-tile map"
	}
	return point, server, ""
}

// atChatTarget is a looked-up card with the point its treasure is on.
type atChatTarget struct {
	card  atChatCard
	point int64
}

func runAllianceTreasuresChat(conn *session.GameConn, in *Init) error {
	res, me, err := chatCards(conn, in)
	if res == nil {
		return err
	}
	errs := []error{err}
	st := tileClaims(conn, in, allianceTreasureFamily)
	if st.stop != "" {
		slog.Info(allianceTreasuresChatFeature+": treasure claims were stopped earlier this run; skipping", "errorCode", st.stop)
		return errors.Join(errs...)
	}
	if !me.atLoginSource() {
		slog.Info(allianceTreasuresChatFeature + ": away from the login source server; the cards send nothing there (season_tips166)")
		return errors.Join(errs...)
	}
	cards := treasureCards(res, me, evNow().UnixMilli(), st.answered)
	budget := atMaxClaimsPerRun - len(st.answered)
	if len(cards) > budget {
		slog.Info(allianceTreasuresChatFeature+": more cards than this run's claim cap leaves; the rest wait for the next run",
			"cards", len(cards), "left", max(budget, 0), "cap", atMaxClaimsPerRun)
		cards = cards[:max(budget, 0)]
	}
	var targets []atChatTarget
	loggedFirst := false
	for i, c := range cards {
		if i > 0 {
			fwSleep(atClaimPace)
		}
		target := me.sourceServer
		if c.t.HasTarget {
			target = c.t.TargetServer
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", c.t.UUID)
		p.PutInt("source", atInfoSourceChat)
		p.PutInt("targetServer", int32(target))
		resp, code, rerr := fwRequest(conn, allianceTreasuresChatFeature+": treasure info", allianceTreasureInfoCmd, p)
		if rerr != nil {
			errs = append(errs, rerr)
			if session.ContainsNonTimeoutNetError(rerr) {
				return errors.Join(errs...) // the game connection is gone
			}
			continue
		}
		point, server, why := atInfoVerdict(resp, code, me)
		if !loggedFirst {
			loggedFirst = true
			cfg, _ := evNum(resp, "cfgId")
			num, _ := evNum(resp, "num")
			created, _ := evNum(resp, "createTime")
			replyServer, _ := evNum(resp, "targetServer")
			slog.Info(allianceTreasuresChatFeature+": first "+allianceTreasureInfoCmd+" reply", "errorCode", code,
				"replyKeys", resp.Keys(), "cfgId", cfg, "num", num, "numPresent", resp.Has("num"), "pointId", point,
				"createTime", created, "targetServer", replyServer, "claimers", len(evObjects(resp, "array")),
				"ownerInfoKeys", evObject(resp, "ownerInfo").Keys())
		}
		slog.Info(allianceTreasuresChatFeature+": treasure info", "uuid", c.t.UUID, "pointId", point, "server", server,
			"claimers", len(evObjects(resp, "array")), "found", why == "", "reason", why)
		if why == "" {
			targets = append(targets, atChatTarget{card: c, point: point})
		}
	}
	if len(targets) == 0 {
		slog.Info(allianceTreasuresChatFeature+": done", "cards", len(cards), "located", 0, "claimed", 0)
		return errors.Join(errs...)
	}
	points := make([]int64, 0, len(targets))
	for _, t := range targets {
		points = append(points, t.point)
	}
	sc, serr := allianceTilePoints(conn, in, allianceTreasuresChatFeature, points, false)
	errs = append(errs, serr)
	if sc == nil || session.ContainsNonTimeoutNetError(serr) {
		return errors.Join(errs...)
	}
	var picks []atTreasure
	for _, t := range targets {
		// The world tap sends the point's PointInfo.uuid to both the claim-info and the claim
		// (UIUtil.lua:4271-4273), and the card's uuid to the same claim-info, so the card names
		// the point by sc.treasure's uuid.
		i := slices.IndexFunc(sc.treasures, func(pt worldPoint) bool { return sc.treasure(pt).uuid == t.card.t.UUID })
		if i < 0 {
			atPoint := slices.IndexFunc(sc.treasures, func(pt worldPoint) bool { return int64(pt.ID) == t.point })
			slog.Info(allianceTreasuresChatFeature+": treasure not on its tile", "uuid", t.card.t.UUID, "pointId", t.point,
				"treasureAtPoint", atPoint >= 0,
				"innerUuidMatches", atPoint >= 0 && sc.treasures[atPoint].Treasure.UUID == t.card.t.UUID)
			continue
		}
		a := sc.treasure(sc.treasures[i])
		why := sc.verdict(a, st.answered)
		tr := a.pt.Treasure
		expiresIn := int64(-1)
		if tr.ExpireTime > 0 {
			expiresIn = int64(evUnix(tr.ExpireTime).Sub(sc.now).Minutes())
		}
		slog.Info(allianceTreasuresChatFeature+": treasure", "uuid", a.uuid, "pointId", a.pt.ID,
			"atCardPoint", int64(a.pt.ID) == t.point, "eventId", tr.EventID, "treasureType", tr.Type,
			"kind", atTreasureTypes[tr.Type], "pointType", a.pt.PointType, "claimed", tr.Claimers(), "maxClaims", a.maxClaim,
			"multiple", tr.Multiple, "complete", tr.Complete, "expiresInMin", expiresIn, "eligible", why == "", "reason", why)
		if why == "" && !slices.ContainsFunc(picks, func(p atTreasure) bool { return p.uuid == a.uuid }) {
			picks = append(picks, a)
		}
	}
	got, cerrs := sc.claimTreasures(conn, allianceTreasuresChatFeature, picks, st)
	errs = append(errs, cerrs...)
	slog.Info(allianceTreasuresChatFeature+": done", "cards", len(cards), "located", len(targets), "eligible", len(picks), "claimed", got)
	return errors.Join(errs...)
}
