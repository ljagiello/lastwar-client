package game

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Letter items' one-time rewards (MASTER.md G31; cov-ui-city F3 and its verdict), OPT-IN, claims
// only. Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet.
//
// Using a bag item of goods type 160 opens the letter window with the item's otherParam, the init
// item's otherPara string (UILWBagMainCtrl.lua:918-934; ItemInfo.lua:98-101). The window decodes it
// as JSON and shows the reward button while its state is nil or toInt(state) == 0; without a
// decodable otherParam it shows none (ThanksLetterItem.lua:158-176; BirthdayLetterItem.lua:181;
// BirthdaySznLetterItem.lua:223). The button sends post.card.receive {itemId: UtfString}, the goods
// id (ThanksLetterItem.lua:245-247; PostCardReceiveMessage.lua:4-7), and the reply carries reward
// and the updated itemObj. 654125, the VIP 18 invitation (DataConfig vip_letter.k2), opens its own
// envelope and its reply converts a skin (PostCardReceiveMessage.lua:16-41): it is never sent.
const (
	lettersFeature   = "letters"
	letterReceiveCmd = "post.card.receive"
	letterVIP18ID    = "654125" // vip_letter.k2, table item 40413
)

// letterItemIDs are the goods of type 160 in tables 40413 except the VIP 18 invitation.
var letterItemIDs = []string{"654120", "654124", "670002", "670003", "670004", "670005", "670006", "670007", "670008", "670009", "670010"}

func init() {
	registerFeature(Feature{
		Name:      lettersFeature,
		Summary:   "OPT-IN: claim the one-time reward of letter items (goods type 160) whose letter still shows its reward button (static-only, not validated live)",
		DefaultOn: false,
		Run:       runLetters,
	})
}

func runLetters(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	due := lettersDue(in)
	if len(due) == 0 {
		return nil
	}
	slog.Info(lettersFeature+": letters with an unclaimed reward", "items", due)
	b := &evBatch{conn: conn}
	for _, id := range due {
		p := sfs.NewSFSObject()
		p.PutUtfString("itemId", id)
		if !evTaskSend(b, lettersFeature+": letter "+id, letterReceiveCmd, p, "itemId", id) {
			break // the first refusal stops the claims
		}
	}
	return b.err()
}

// lettersDue lists the letter goods ids in init items whose otherPara decodes and has no state or
// state 0, once each.
func lettersDue(in *Init) []string {
	seen := map[string]bool{}
	var due []string
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		id := evString(it, "itemId")
		if id == "" {
			id = evString(it, "goodsId")
		}
		if id == letterVIP18ID || !slices.Contains(letterItemIDs, id) || seen[id] {
			continue
		}
		// ItemInfo:UpdateInfo applies count, num, then itemLeftCnt: the last present one is the
		// stack's count (ItemInfo.lua:52-66).
		if n, ok := claimFirstInt(it, "itemLeftCnt", "num", "count"); ok && n <= 0 {
			continue
		}
		if letterRewardShown(evString(it, "otherPara")) {
			seen[id] = true
			due = append(due, id)
		}
	}
	return due
}

// letterRewardShown is RefreshItemRewardState over otherParam: it decodes to a JSON object whose
// state is absent, null or 0 (a number or a decimal string).
func letterRewardShown(otherPara string) bool {
	if strings.TrimSpace(otherPara) == "" {
		return false
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(otherPara), &data); err != nil {
		return false
	}
	switch st := data["state"].(type) {
	case nil:
		return true
	case float64:
		return int64(st) == 0
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(st), 64)
		return err == nil && int64(n) == 0
	}
	return false
}
