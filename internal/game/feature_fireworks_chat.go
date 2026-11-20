package game

import (
	"cmp"
	"errors"
	"log/slog"
	"slices"
	"time"

	"lastwar-client/internal/chat"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// fireworks-chat (OPT-IN, static-only): claim the firework chests the alliance chat's firework
// cards point at. fireworks finds chests on the member HQ tiles only; the S0 alliance-boss chests
// (firework.type 1, ids 671001-671005 in 40413) sit on alliance buildings, which that scan reads
// only when one stands near a member HQ. Their card, post 695, names the chest.
//
// The cards come from this run's one chat read (chatCards, shared with chat-scan, red-packets and
// alliance-treasures-chat). A tap on a card sends
//
//	find.fireworks.gift.world.point {uuid: Long, ownerUid: Utf}
//
// with uuid and ownerUid from the card's customJsonParam (FindFireworksGiftWorldPointMessage.lua:
// 4-8), and the reply {currServerId, pointId} moves the camera there; pointId 0 is the tip
// firework_tips_1018, the chest is gone (:10-22). On the map the chest's building shows its
// bubble, and a tap on it is fireworks' own claim (FireworkWorldBuildBubble.lua:117-131). So this
// sends the lookup per card, reads the blocks holding the points (allianceTilePoints), and applies
// fireworks' rules to the chest with the card's uuid on those tiles (fwScan.eligible: sent, under
// 120 minutes old, not full, an owner, type 0 on a member's building, type 1 on this alliance's)
// before the same get.fireworks.gift claim (fwScan.claimChests).
//
// A card is looked up when, as for the card's button:
//   - post 693 (ChatItemPost_FireworkGift, FireworkGift.lua:22-43, 58-130): not overdue, its
//     expiry being the message's serverTime + DataConfig fireworks.k2 (120) minutes (:66-67;
//     a card without serverTime is skipped), and the player is not in the card's claimer list
//     (clientUpdateExtra split on "|", :97-103; a claimer opens the record window instead). The
//     button's other test, the claimers reaching firework.reward[index+1]'s third field, needs
//     the firework table, which Go does not generate: the tile's num and max decide instead;
//   - post 695 (ChatItemS0AllianceBossPushMsg.lua:34-61): not overdue by the card's own
//     expiredTime (ms; absent or 0 never expires). The button also needs
//     PlayerInfo:IsInSelfServer (checkServerId < 0 or the self server, PlayerInfo.lua:840-842),
//     which holds for a session that never views another server (the C# crossServerId starts at
//     -1, A-CS:41272): always true here;
//   - both: the card has a uuid and an ownerUid (the lookup sends both), and this run has not
//     answered the chest yet (fireworks, run just before by name, shares tileClaims), nor did a
//     daily cap or slow-down stop this run's firework claims.
//
// The reply's chest is read only on the player's own server and the 1000-tile map: the client
// would view currServerId's map, whose block requests also send world.get.cross
// (SendAoiRequest, A-CS:179953-179999; WorldCrossServerMessage, A-CS:84461-84492), and Go reads
// the own map only. A lookup's errorCode skips that card; no code is registered as benign.
// Lookups take the newest cards first, fireworksClaimPace apart, at most fwMaxClaimsPerRun less
// the chests this run already answered. A claim is as visible as fireworks' (the server may show
// it over the base and post a double reward in alliance chat); the read itself never posts.
// Unverified live: the lookup's reply, and whether the server needs the chest's block in view.

const (
	fireworksChatFeature = "fireworks-chat"
	fireworksFindCmd     = "find.fireworks.gift.world.point"
)

func init() {
	registerFeature(Feature{
		Name: fireworksChatFeature,
		Summary: "OPT-IN: claim the firework chests the alliance chat's firework cards point at, the S0 alliance-boss chests " +
			"on alliance buildings among them (find.fireworks.gift.world.point, the chest's world block, get.fireworks.gift, free); " +
			"reads chat, never posts; the claim shows like fireworks' (static-only, not validated live)",
		Run: runFireworksChat,
	})
}

// fwChatCard is one firework card to look up.
type fwChatCard struct {
	post chat.Post
	f    chat.Firework
}

// fireworkCardVerdict decodes a post 693 or 695 card and applies the button's rules at now; "" is
// look it up. answered holds the chests this run already answered.
func fireworkCardVerdict(p chat.Post, me chatSelf, now time.Time, answered map[int64]bool) (chat.Firework, string) {
	f, err := p.Firework()
	if err != nil {
		return f, "unreadable card: " + err.Error()
	}
	nowMs := now.UnixMilli()
	switch {
	case f.OwnerUID == "":
		return f, "no ownerUid"
	case answered[f.UUID]:
		return f, "already answered this run"
	case slices.Contains(f.Claimers, me.uid):
		return f, "already claimed by you"
	case p.Type == chat.PostFireworkGift && p.ServerTimeMs <= 0:
		return f, "no serverTime"
	case p.Type == chat.PostFireworkGift && !now.Before(time.UnixMilli(p.ServerTimeMs).Add(chatFireworkLifetime)):
		return f, "expired"
	case p.Type == chat.PostS0BossFireworks && f.ExpiredTime > 0 && f.ExpiredTime <= nowMs:
		return f, "expired"
	}
	return f, ""
}

// fireworkCards logs every firework card with its verdict and returns those to look up, newest
// first, each chest once.
func fireworkCards(res *chat.Result, me chatSelf, now time.Time, answered map[int64]bool) []fwChatCard {
	var out []fwChatCard
	seen := map[int64]bool{}
	for _, p := range res.Posts {
		if p.Type != chat.PostFireworkGift && p.Type != chat.PostS0BossFireworks {
			continue
		}
		f, why := fireworkCardVerdict(p, me, now, answered)
		if why == "" && seen[f.UUID] {
			why = "duplicate card"
		}
		ageMin := int64(-1)
		if p.ServerTimeMs > 0 {
			ageMin = (now.UnixMilli() - p.ServerTimeMs) / 60000
		}
		slog.Info(fireworksChatFeature+": firework card", "post", p.Type, "seqId", p.SeqID, "uuid", f.UUID,
			"configId", f.ConfigID, "type", fwType(int32(f.ConfigID)), "index", f.Index, "sender", senderRef(p, me),
			"claimers", len(f.Claimers), "ageMin", ageMin, "lookUp", why == "", "reason", why)
		if why == "" {
			seen[f.UUID] = true
			out = append(out, fwChatCard{post: p, f: f})
		}
	}
	slices.SortStableFunc(out, func(a, b fwChatCard) int {
		return cmp.Or(cmp.Compare(b.post.ServerTimeMs, a.post.ServerTimeMs), cmp.Compare(b.post.SeqID, a.post.SeqID))
	})
	return out
}

// fwChatTarget is a looked-up card with the point its chest is on.
type fwChatTarget struct {
	card  fwChatCard
	point int64
}

func runFireworksChat(conn *session.GameConn, in *Init) error {
	res, me, err := chatCards(conn, in)
	if res == nil {
		return err
	}
	errs := []error{err}
	st := tileClaims(conn, in, fireworksFamily)
	if st.stop != "" {
		slog.Info(fireworksChatFeature+": firework claims were stopped earlier this run; skipping", "errorCode", st.stop)
		return errors.Join(errs...)
	}
	cards := fireworkCards(res, me, evNow(), st.answered)
	budget := fwMaxClaimsPerRun - len(st.answered)
	if len(cards) > budget {
		slog.Info(fireworksChatFeature+": more cards than this run's claim cap leaves; the rest wait for the next run",
			"cards", len(cards), "left", max(budget, 0), "cap", fwMaxClaimsPerRun)
		cards = cards[:max(budget, 0)]
	}
	var targets []fwChatTarget
	loggedFirst := false
	for i, c := range cards {
		if i > 0 {
			fwSleep(fireworksClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", c.f.UUID)
		p.PutUtfString("ownerUid", c.f.OwnerUID)
		resp, code, rerr := fwRequest(conn, fireworksChatFeature+": chest lookup", fireworksFindCmd, p)
		if rerr != nil {
			errs = append(errs, rerr)
			if session.ContainsNonTimeoutNetError(rerr) {
				return errors.Join(errs...) // the game connection is gone
			}
			continue
		}
		cur, hasCur := evNum(resp, "currServerId")
		point, hasPoint := evNum(resp, "pointId")
		if !loggedFirst {
			loggedFirst = true
			slog.Info(fireworksChatFeature+": first "+fireworksFindCmd+" reply", "errorCode", code, "replyKeys", resp.Keys(),
				"currServerId", cur, "pointId", point)
		}
		why := ""
		switch {
		case code != "":
			why = "lookup refused: " + code
		case !hasCur || !hasPoint:
			why = "reply without currServerId or pointId"
		case point == 0:
			why = "chest gone (firework_tips_1018)"
		case cur != me.selfServer:
			why = "on another server"
		}
		if why == "" {
			if _, _, ok := fwTilePos(point); !ok {
				why = "outside the 1000-tile map"
			}
		}
		slog.Info(fireworksChatFeature+": chest lookup", "post", c.post.Type, "uuid", c.f.UUID, "pointId", point,
			"onOwnServer", cur == me.selfServer, "found", why == "", "reason", why)
		if why == "" {
			targets = append(targets, fwChatTarget{card: c, point: point})
		}
	}
	if len(targets) == 0 {
		slog.Info(fireworksChatFeature+": done", "cards", len(cards), "located", 0, "claimed", 0)
		return errors.Join(errs...)
	}
	points := make([]int64, 0, len(targets))
	for _, t := range targets {
		points = append(points, t.point)
	}
	sc, serr := allianceTilePoints(conn, in, fireworksChatFeature, points, true)
	errs = append(errs, serr)
	if sc == nil || session.ContainsNonTimeoutNetError(serr) {
		return errors.Join(errs...)
	}
	var picks []fwBox
	for _, t := range targets {
		i := slices.IndexFunc(sc.boxes, func(b fwBox) bool { return b.UUID == t.card.f.UUID })
		if i < 0 {
			slog.Info(fireworksChatFeature+": chest not on its tile", "uuid", t.card.f.UUID, "pointId", t.point)
			continue
		}
		b := sc.boxes[i]
		ok, why := sc.eligible(b, st.answered)
		slog.Info(fireworksChatFeature+": chest", "uuid", b.UUID, "post", t.card.post.Type, "owner", sc.ref(b.OwnerUID),
			"tileOwnerPresent", b.OwnerUID != "", "ownerMatchesCard", b.OwnerUID == t.card.f.OwnerUID,
			"configId", b.ConfigID, "type", fwType(b.ConfigID), "num", b.Num, "max", b.Max,
			"ageMin", int64(sc.now.Sub(evUnix(b.SendTime)).Minutes()), "eligible", ok, "reason", why)
		if ok && !slices.ContainsFunc(picks, func(p fwBox) bool { return p.UUID == b.UUID }) {
			picks = append(picks, b)
		}
	}
	got, cerrs := sc.claimChests(conn, fireworksChatFeature, picks, st)
	errs = append(errs, cerrs...)
	slog.Info(fireworksChatFeature+": done", "cards", len(cards), "located", len(targets), "eligible", len(picks), "claimed", got)
	return errors.Join(errs...)
}
